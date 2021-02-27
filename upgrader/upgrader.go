package upgrader

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// DefaultUpgradeTimeout is the duration before the Upgrader kills the new process if no
// readiness notification was received.
const DefaultUpgradeTimeout time.Duration = time.Minute

// Options control the behaviour of the Upgrader.
type Options struct {
	// Time after which an upgrade is considered failed. Defaults to
	// DefaultUpgradeTimeout.
	UpgradeTimeout time.Duration
	// The PID of a ready process is written to this file.
	PIDFile string
	// ListenConfig is a custom ListenConfig. Defaults to an empty ListenConfig
	ListenConfig *net.ListenConfig
}

// Upgrader handles zero downtime upgrades and passing files between processes.
type Upgrader struct {
	*Fds

	*env
	opts      Options
	parent    *parent
	parentErr chan error
	readyOnce sync.Once
	readyC    chan struct{}
	stopOnce  sync.Once
	stopC     chan struct{}
	upgradeC  chan chan<- error
	exitC     chan struct{}
	exitFd    chan neverCloseThisFile
}

var (
	stdEnvMu       sync.Mutex
	stdEnvUpgrader *Upgrader
)

var ErrNotSupported = errors.New("upgrader: platform does not support graceful restart")

// 新建将创建一个新的升级程序。文件是从父级传递的，可能为空。
//只有第一次对该函数的调用将成功。可能返回Err Not Supported。
func New(opts Options) (upg *Upgrader, err error) {
	stdEnvMu.Lock()
	defer stdEnvMu.Unlock()

	// 只支持类Unix系统
	if !isSupportedOS() {
		return nil, fmt.Errorf("%w", ErrNotSupported)
	}

	if stdEnvUpgrader != nil {
		return nil, errors.New("upgrader: only a single Upgrader allowed")
	}

	upg, err = newUpgrader(stdEnv, opts)
	//将对upg的引用存储在私有全局变量中，以防止对它进行GC和过早关闭exitS。
	stdEnvUpgrader = upg
	return
}

func newUpgrader(env *env, opts Options) (*Upgrader, error) {
	// 获取当前的工作目录
	if initialWD == "" {
		return nil, errors.New("couldn't determine initial working directory")
	}

	parent, files, err := newParent(env)
	if err != nil {
		return nil, err
	}

	if opts.UpgradeTimeout <= 0 {
		opts.UpgradeTimeout = DefaultUpgradeTimeout
	}

	u := &Upgrader{
		env:       env,
		opts:      opts,
		parent:    parent,
		parentErr: make(chan error, 1),
		readyC:    make(chan struct{}),
		stopC:     make(chan struct{}),
		upgradeC:  make(chan chan<- error),
		exitC:     make(chan struct{}),
		exitFd:    make(chan neverCloseThisFile, 1),
		Fds:       newFds(files, opts.ListenConfig),
	}

	go u.run()

	return u, nil
}


//表示当前进程已准备好接受连接。
//必须调用它才能完成升级。
//调用Ready之后，将关闭所有继承但未使用的fds。
func (u *Upgrader) Ready() error {
	u.readyOnce.Do(func() {
		u.Fds.closeInherited()
		close(u.readyC)
	})

	if u.opts.PIDFile != "" {
		if err := writePIDFile(u.opts.PIDFile); err != nil {
			return fmt.Errorf("upgrader: can't write PID file: %s", err)
		}
	}

	if u.parent == nil {
		return nil
	}
	return u.parent.sendReady()
}

// Exit returns a channel which is closed when the process should
// exit.
func (u *Upgrader) Exit() <-chan struct{} {
	return u.exitC
}

// Stop prevents any more upgrades from happening, and closes
// the exit channel.
//
// If this function is called before a call to Upgrade() has
// succeeded, it is assumed that the process is being shut down
// completely. All Unix sockets known to Upgrader.Fds are then
// unlinked from the filesystem.
func (u *Upgrader) Stop() {
	u.stopOnce.Do(func() {
		// Interrupt any running Upgrade(), and
		// prevent new upgrade from happening.
		close(u.stopC)
	})
}

// WaitForParent blocks until the parent has exited.
//
// Returns an error if the parent misbehaved during shutdown.
func (u *Upgrader) WaitForParent(ctx context.Context) error {
	if u.parent == nil {
		return nil
	}

	var err error
	select {
	case err = <-u.parent.result:
	case err = <-u.parentErr:
	case <-ctx.Done():
		return ctx.Err()
	}

	// This is a bit cheeky, since it means that multiple
	// calls to WaitForParent resolve in sequence, but that
	// probably doesn't matter.
	u.parentErr <- err
	return err
}

// HasParent checks if the current process is an upgrade or the first invocation.
func (u *Upgrader) HasParent() bool {
	return u.parent != nil
}

// Upgrade triggers an upgrade.
func (u *Upgrader) Upgrade() error {
	response := make(chan error, 1)
	select {
	case <-u.stopC:
		return errors.New("terminating")
	case <-u.exitC:
		return errors.New("already upgraded")
	case u.upgradeC <- response:
	}

	return <-response
}

var errNotReady = errors.New("process is not ready yet")

func (u *Upgrader) run() {
	defer close(u.exitC)

	var (
		parentExited <-chan struct{}
		processReady = u.readyC
	)

	if u.parent != nil {
		parentExited = u.parent.exited
	}

	for {
		select {
		case <-parentExited:
			log.Println("<-parentExited")
			parentExited = nil

		case <-processReady:
			log.Println("<-processReady")
			processReady = nil

		case <-u.stopC:
			log.Println("<-u.stopC")
			u.Fds.closeAndRemoveUsed()
			return

		case request := <-u.upgradeC:
			log.Println("<-u.upgradeC")
			if processReady != nil {
				request <- errNotReady
				continue
			}

			if parentExited != nil {
				request <- errors.New("parent hasn't exited")
				continue
			}

			file, err := u.doUpgrade()
			request <- err

			if err == nil {
				// 将文件保存在excitEd中，以便仅在进程退出时关闭文件。
				// 这向新进程发出信号，表明旧进程已退出。
				u.exitFd <- neverCloseThisFile{file}
				u.Fds.closeUsed()
				return
			}
		}
	}
}

func (u *Upgrader) doUpgrade() (*os.File, error) {

	child, err := startChild(u.env, u.Fds.copy())

	if err != nil {
		return nil, fmt.Errorf("can't start child: %s", err)
	}

	readyTimeout := time.After(u.opts.UpgradeTimeout)
	for {
		select {
		case request := <-u.upgradeC:
			request <- errors.New("upgrade in progress")

		case err := <-child.result:
			if err == nil {
				return nil, fmt.Errorf("child %s exited", child)
			}
			return nil, fmt.Errorf("child %s exited: %s", child, err)

		case <-u.stopC:
			child.Kill()
			return nil, errors.New("terminating")

		case <-readyTimeout:
			child.Kill()
			return nil, fmt.Errorf("new child %s timed out", child)

		case file := <-child.ready:
			return file, nil
		}
	}
}

// This file must never be closed by the Go runtime, since its used by the
// child to determine when the parent has died. It must only be closed
// by the OS.
// Hence we make sure that this file can't be garbage collected by referencing
// it from an Upgrader.
type neverCloseThisFile struct {
	file *os.File
}

func writePIDFile(path string) error {
	dir, file := filepath.Split(path)

	// if dir is empty, the user probably specified just the name
	// of the pid file expecting it to be created in the current work directory
	if dir == "" {
		dir = initialWD
	}

	if dir == "" {
		return errors.New("empty initial working directory")
	}

	fh, err := ioutil.TempFile(dir, file)
	if err != nil {
		return err
	}
	defer fh.Close()
	// Remove temporary PID file if something fails
	defer os.Remove(fh.Name())

	_, err = fh.WriteString(strconv.Itoa(os.Getpid()))
	if err != nil {
		return err
	}

	return os.Rename(fh.Name(), path)
}

// Check if this is a supported OS.
// That is currently all Unix-like OS's.
// At the moment, we assume that is everything except Windows.
func isSupportedOS() bool {
	return runtime.GOOS != "windows"
}
