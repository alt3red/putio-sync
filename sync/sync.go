package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/igungor/go-putio/putio"
	"github.com/rjeczalik/notify"
)

// temporary extension to indicate that the file is still not downloaded.
const inProgressExtension = ".putdl"
const defaultUserAgent = "putio-sync"

type Client struct {
	// Logging facility
	*Logger

	// Debug flag for logger verbosity
	Debug bool

	// Configuration
	Config *Config

	// Putio API client
	C *putio.Client

	// Authenticated user
	User *putio.AccountInfo

	// Database handle
	Store *Store

	// Currently running tasks
	Tasks *Tasks

	// Context for various signalling purposes
	Ctx context.Context

	// mu guards CancelFunc access
	mu sync.Mutex

	// Global cancellation function.
	//
	// Calling CancelFunc stops all the running tasks.
	//
	// It is also an indicator to the current state of the Client. If it is
	// nil, it means that client has stopped its poll/download cycle. Else, it
	// actively polls for files and downloads them in the background if there
	// are any new files exist.
	CancelFunc context.CancelFunc

	// Serves as a job queue
	taskCh chan *Task

	// Channel to communicate when all tasks are done.
	//
	// doneCh is a volatile channel, meaning that client creates the channel on
	// every Run(). It should not be created at initializtion.
	doneCh chan struct{}

	// Channel to listen to filesystem events for torrents folder
	torrentsCh chan notify.EventInfo
}

func NewClient(debug bool) (*Client, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}

	appPath := filepath.Join(u.HomeDir, ".putio-sync")
	err = os.MkdirAll(appPath, 0755)
	if err != nil {
		return nil, err
	}

	cfgpath := filepath.Join(appPath, "putio-sync.db")
	store := NewStore(cfgpath)

	err = store.Open()
	if err != nil {
		return nil, err
	}

	cfg, err := store.Config("")
	if err != nil {
		return nil, err
	}

	oauthClient := oauth2.NewClient(
		oauth2.NoContext,
		oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: cfg.OAuth2Token},
		),
	)
	client := putio.NewClient(oauthClient)
	client.UserAgent = defaultUserAgent

	return &Client{
		Logger: NewLogger("sync: ", debug, appPath),
		Debug:  debug,
		Config: cfg,
		C:      client,
		User:   nil,
		Store:  store,
		Tasks:  NewTasks(),
		taskCh: make(chan *Task),
		// Make the channel buffered to ensure no event is dropped.
		// Notify will drop an event if the receiver is not able to
		// keep up the sending pace.
		torrentsCh: make(chan notify.EventInfo, 1),
	}, nil
}

// Run starts watching the directory and consuming the download tasks.
func (c *Client) Run() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.CancelFunc != nil {
		return Error("already running")
	}

	if c.Config.DownloadFrom < 0 {
		return Error("Invalid Put.io folder ID")
	}

	if c.Config.OAuth2Token == "" {
		return Error("OAuth2 token not found")
	}

	if c.User == nil {
		return Error("No authenticated user found")
	}

	if c.Config.IsPaused {
		c.Config.IsPaused = false
		c.Store.SaveConfig(c.Config, c.User.Username)
	}

	// assign the cancellation function to indicate that the client is already
	// runnign and is cancellable.
	c.Ctx, c.CancelFunc = context.WithCancel(context.Background())
	c.doneCh = make(chan struct{})

	go c.queueTasks(c.Ctx)
	go c.runConsumers(c.Ctx)

	return nil
}

// Stop halts all running tasks in a graceful way.
func (c *Client) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.CancelFunc == nil {
		return Error("already stopped")
	}

	// cancel the poll/download cycle.
	c.CancelFunc()

	// wait for all the active running tasks to catch the cancellation signal.
	<-c.doneCh

	if !c.Config.IsPaused {
		c.Config.IsPaused = true
		c.Store.SaveConfig(c.Config, c.User.Username)
	}

	// reset cancellation states for fresh start.
	c.CancelFunc = nil
	c.doneCh = nil

	return nil
}

// Close releases all the resources, closes database connections and file
// handles.
func (c *Client) Close() error {
	// unregister from watching filesystem events
	notify.Stop(c.torrentsCh)

	// stop all current tasks
	err := c.Stop()
	if err != nil {
		return err
	}

	// release the database handle
	err = c.Store.Close()
	if err != nil {
		return err
	}

	return c.Logger.Close()
}

// Status returns current state of the synchronization client.
func (c *Client) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.CancelFunc == nil {
		return "stopped"
	}

	if c.Tasks.Empty() {
		return "up-to-date"
	}

	return "syncing"
}

// RenewToken creates a new OAuth2 enabled HTTP client for the stored token.
// This method is used for changing the OAuth2 token of the Client without
// restarting the application.
func (c *Client) RenewToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Config.OAuth2Token == "" {
		c.Println("Token is empty, ignoring...")
		return fmt.Errorf("OAuth2 token is empty")
	}

	oauthClient := oauth2.NewClient(
		oauth2.NoContext,
		oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: c.Config.OAuth2Token},
		),
	)

	client := putio.NewClient(oauthClient)
	client.UserAgent = defaultUserAgent
	c.C = client

	user, err := c.C.Account.Info(nil)
	if err != nil {
		return err
	}
	c.User = &user

	err = c.Store.SaveCurrentUser(user.Username)
	if err != nil {
		return err
	}

	return c.Store.CreateBuckets(c.User.Username)
}

// DeleteToken deletes the token associated with the Client.
func (c *Client) DeleteToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Config.OAuth2Token = ""
	err := c.Store.SaveConfig(c.Config, c.User.Username)
	if err != nil {
		return err
	}

	c.User = nil

	return c.Store.SaveCurrentUser("")
}

func (c *Client) queueTasks(ctx context.Context) {
	const rootFolder = "/"
	c.walk(ctx, c.Config.DownloadFrom, rootFolder)

	for {
		select {
		case <-time.After(time.Duration(c.Config.PollInterval)):
			c.walk(ctx, c.Config.DownloadFrom, rootFolder)
		case <-ctx.Done():
			return
		}
	}
}

// walk recursively walks the put.io filesystem, starting from the given
// putioFolderID. All files are pushed to a task channel to be consumed.
func (c *Client) walk(ctx context.Context, putioFolderID int64, cwd string) {
	files, _, err := c.C.Files.List(ctx, putioFolderID)
	if err != nil {
		return
	}

	for _, file := range files {
		if file.IsDir() {
			newcwd := filepath.Join(cwd, file.Name)
			c.walk(ctx, file.ID, newcwd)
			continue
		}

		t := NewTask(file, cwd, c.Config.DownloadTo)

		select {
		case <-ctx.Done():
			c.Debugf("Context cancelled: %v\n", ctx.Err())
			return
		case c.taskCh <- t:
			c.Debugf("Adding %v to queue\n", t)
		}
	}
}

func (c *Client) runConsumers(ctx context.Context) {
	var wg sync.WaitGroup
	for i := uint(0); i < c.Config.MaxParallelFiles; i++ {
		wg.Add(1)
		go c.consumeTasks(ctx, &wg)
	}

	wg.Wait()
	close(c.doneCh)
}

func (c *Client) consumeTasks(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-c.taskCh:
			// skip running tasks
			if c.Tasks.Exists(t) {
				continue
			}

			c.Tasks.Add(t)
			c.processTask(ctx, t)
			c.Tasks.Remove(t)
		}
	}
}

func (c *Client) processTask(ctx context.Context, t *Task) {
	// search for previous states
	state, err := c.Store.State(int64(t.state.FileID), c.User.Username)
	if err != nil && err != ErrStateNotFound {
		c.Printf("Error retrieving state for file %q: %v\n", t.state.FileID, err)
		return
	}

	if err == ErrStateNotFound {
		c.Debugf("State not found for file %q, using the fresh state\n", t.state.FileName)
	} else {
		c.Debugf("Existing state found for file %q, resuming... \n", t.state.FileName)
		t.state = state
	}

	// skip already synced tasks
	if t.state.DownloadStatus == DownloadCompleted {
		return
	}

	t.chunks = calculateChunks(t.state, c.Config.SegmentsPerFile)

	err = c.download(ctx, t)
	if err == context.Canceled {
		c.Debugf("Task %q cancelled by request\n", t)
		return
	}

	if err != nil {
		c.Printf("Error downloading %q. err: %v\n", t, err)
		return
	}

	if c.Config.DeleteRemoteFile {
		err = c.C.Files.Delete(ctx, t.state.FileID)
		if err != nil {
			c.Printf("File %v successfully downloaded but the remote file could not be deleted: %v\n", t, err)
		}
	}
	c.Printf("File %v successfully downloaded\n", t)
}

// download fetches the given task, splits into multiple chunks and downloads
// them concurrently.
func (c *Client) download(ctx context.Context, t *Task) error {
	c.Debugf("Starting to download: %v\n", t)

	// parent directory of the file
	taskdir := filepath.Join(filepath.Clean(c.Config.DownloadTo), t.cwd)
	// absolute path of the file, with an extension added, indicating that the
	// file is not completed yet.
	taskpath := filepath.Join(taskdir, t.state.FileName)
	taskpath += inProgressExtension

	_, err := os.Stat(taskdir)
	if os.IsNotExist(err) {
		err = os.MkdirAll(taskdir, 0755|os.ModeDir)
		if err != nil {
			return err
		}
	}

	f, err := os.OpenFile(taskpath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// pre-allocate file space. It's ok if it fails.
	_ = Preallocate(f, t.state.FileLength)

	t.state.DownloadStartedAt = time.Now()
	t.state.DownloadStatus = DownloadInProgress
	t.state.BytesTransferredSinceLastUpdate = 0

	err = c.Store.SaveState(t.state, c.User.Username)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, ch := range t.chunks {
		ch := ch // https://golang.org/doc/faq#closures_and_goroutines
		g.Go(func() error {
			return c.downloadRange(ctx, f, t, ch)
		})
	}

	err = g.Wait()
	if err != nil {
		switch err {
		case context.Canceled:
			t.state.DownloadStatus = DownloadPaused
		default:
			t.state.DownloadStatus = DownloadFailed
			t.state.Error = err.Error()
		}
		_ = c.Store.SaveState(t.state, c.User.Username)
		return err
	}

	err = t.Verify(f)
	if err != nil {
		c.Printf("verification failed for %v: %v\n", t, err)
		t.state.DownloadStatus = DownloadFailed
		t.state.Error = err.Error()
		_ = c.Store.SaveState(t.state, c.User.Username)
		return err
	}

	// Rename the file to its original name after a successful download operation
	err = os.Rename(taskpath, strings.TrimSuffix(taskpath, inProgressExtension))
	if err != nil {
		return err
	}

	// all chunks are downloaded and verified
	t.state.DownloadStatus = DownloadCompleted
	t.state.DownloadFinishedAt = time.Now().UTC()
	t.state.Error = ""
	return c.Store.SaveState(t.state, c.User.Username)
}

func (c *Client) downloadRange(ctx context.Context, w io.WriterAt, t *Task, ch *chunk) error {
	body, err := c.doRequest(ctx, t, ch)
	if err != nil {
		c.Debugf("Error retrieving body for %q/%q: %v\n", ch, t, err)
		return err
	}

	return c.copyChunk(w, body, ch, t.state)
}

func (c *Client) doRequest(ctx context.Context, t *Task, ch *chunk) (io.ReadCloser, error) {
	rangeHeader := http.Header{}
	// 0 byte files cannot be retrieved with a range request. Servers will
	// return "416 - Requested Range Not Satisfiable".
	// Set the boundry only if the file has content.
	if t.state.FileLength != 0 {
		rangeHeader.Set("Range", fmt.Sprintf("bytes=%v-%v", ch.offset, ch.offset+ch.length-1))
	}

	return c.C.Files.Download(ctx, t.state.FileID, false, rangeHeader)
}

func (c *Client) copyChunk(w io.WriterAt, body io.ReadCloser, ch *chunk, state *State) error {
	c.Debugf("Copying %v of %v\n", ch, state.FileName)

	defer body.Close()

	var n int64
	bfPieceLength := int64(state.BitfieldPieceLength)
	buf := make([]byte, bfPieceLength)

	for curoffset := ch.offset; curoffset < ch.offset+ch.length; curoffset += n {
		idx := curoffset / bfPieceLength                    // bitfield index
		n = (ch.offset + ch.length) - (idx * bfPieceLength) // read this amount of bytes
		if n > bfPieceLength {
			n = bfPieceLength
		}

		written, err := io.ReadFull(body, buf[:n])
		if err != nil {
			c.Debugf("Error copying body: %v\n", err)
			return err
		}

		_, err = w.WriteAt(buf[:n], curoffset)
		if err != nil {
			c.Debugf("Error writing body at offset %v: %v\n", curoffset, err)
			return err
		}

		state.mu.Lock()
		state.BytesTransferredSinceLastUpdate += int64(written)
		state.Bitfield.Set(uint32(idx))
		state.mu.Unlock()

		err = c.Store.SaveState(state, c.User.Username)
		if err != nil {
			return err
		}
	}

	c.Debugf("Copying %q of %q success\n", ch, state.FileName)
	return nil
}

// WatchTorrentFolder registers a watcher for the user's preferred
// TorrentsFolder.
func (c *Client) WatchTorrentFolder() {
	if !c.Config.WatchTorrentsFolder {
		return
	}

	if c.Config.TorrentsFolder == "" {
		c.Println("No torrent folder is given")
		return
	}

	// watch for create and rename events, since moving from one folder to
	// another is simpy a 'rename' event.
	err := notify.Watch(c.Config.TorrentsFolder, c.torrentsCh, notify.Create, notify.Rename)
	if err != nil {
		c.Printf("error watching torrent fodler: %v\n", err)
		return
	}

	uploadTorrentFunc := func(path string) error {
		path = filepath.Clean(path)

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("Error opening file: %v", err)
		}
		defer f.Close()

		_, filename := filepath.Split(path)
		u, err := c.C.Files.Upload(nil, f, filename, -1)
		if err != nil {
			return fmt.Errorf("Error uploading file: %v", err)
		}

		if u.Transfer == nil {
			return fmt.Errorf("API hasn't started the transfer for some reason")
		}

		err = os.RemoveAll(path)
		if err != nil {
			return fmt.Errorf("Error removing file: %v", err)
		}

		return nil

	}

	// Perform an initial scan on the directory
	err = filepath.Walk(c.Config.TorrentsFolder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !strings.HasSuffix(info.Name(), ".torrent") {
			return nil
		}

		c.Debugf("found '%v' on initial scan\n", info.Name())

		return uploadTorrentFunc(path)
	})

	if err != nil {
		c.Printf("Error walking the torrent folder: %v\n", err)
	}

	for {
		select {
		case event := <-c.torrentsCh:
			path := event.Path()

			if !strings.HasSuffix(path, ".torrent") {
				continue
			}

			// if a file is renamed, it might have been moved from someplace
			// else. so check if the file exists, and skip the simple
			// 'renaming' events.
			if event.Event() == notify.Rename && !exists(path) {
				continue
			}

			c.Debugf("New event: %v, %v\n", event.Event(), path)

			err = uploadTorrentFunc(path)
			if err != nil {
				c.Printf("Error uploading torrent file: %v\n", err)
				return
			}
		}
	}
}

func exists(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}
