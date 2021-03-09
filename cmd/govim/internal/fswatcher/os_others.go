//go:build !darwin
// +build !darwin

package fswatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/tomb.v2"
)

type fswatcher struct {
	eventCh chan Event
	errCh   chan error
	mw      *fsnotify.Watcher
	logf    LogFn
}

func New(root string, skipDir WatchFilterFn, logf LogFn, tomb *tomb.Tomb) (*FSWatcher, error) {
	mw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create new watcher: %v", err)
	}

	// activeWatchers is a map keyed by directory paths for directories that has been
	// added to watch. The bool value is used to dedup multiple remove events for the same
	// directory from fsnotify (inotify sends two events when removing a dir, "DELETE_SELF"
	// and "DELETE,ISDIR" where the difference isn't exposed by fsnotify).
	activeWatches := make(map[string]bool)
	eventCh := make(chan Event)

	w := &FSWatcher{&fswatcher{
		eventCh: eventCh,
		errCh:   mw.Errors,
		mw:      mw,
		logf:    logf,
	}}

	var populateWatchers func(string) ([]string, error)
	populateWatchers = func(initPath string) (files []string, err error) {
		return files, filepath.Walk(initPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// fast path for directories that are already watched
			if activeWatches[path] {
				return nil
			}

			if info.IsDir() && skipDir(path) {
				// We might end up here if the user creates a go.mod in a watched dir
				// for example. Then we need to remove the watch.
				if v, ok := activeWatches[path]; ok && v {
					logf("stopped watching dir %q", path)
					mw.Remove(path)
					activeWatches[path] = false
				}
				return filepath.SkipDir
			}

			if !info.IsDir() {
				files = append(files, path)
				return nil
			}

			if v, ok := activeWatches[path]; !ok || !v {
				if err := mw.Add(path); err != nil {
					return err
				}
				logf("started watching dir %q", path)
				activeWatches[path] = true
				// When mw.Add returns the folder might already contain files and/or
				// directories that we must send events for manually. The recommended
				// way in inotify(7) is to:
				//
				// "[...] new files (and subdirectories) may already exist inside the
				// subdirectory. Therefore, you might want to scan the contents of ths
				// subdirectory immediately after adding the watch (and, if desired,
				// recursively add watchers for any subdirectories that it contains).
				fs, err := populateWatchers(path)
				files = append(files, fs...)
				if err != nil {
					return err
				}
				// We must stop walking here since we initiated a new walk above, to avoid
				// duplicate events..
				return filepath.SkipDir
			}
			return nil
		})
	}
	if _, err := populateWatchers(root); err != nil {
		return nil, fmt.Errorf("initial root walk failed: %w", err)
	}

	tomb.Go(func() error {
		for {
			e, ok := <-mw.Events
			if !ok {
				break
			}
			if os.Getenv("GOVIM_FILEWATCHER_DEBUG") == "true" {
				logf("raw %s", e)
			}
			path := e.Name
			switch e.Op {
			// fsnotify processes file renaming as a Rename event followed by a
			// Create event, so we can effectively treat renaming as removal.
			case fsnotify.Rename, fsnotify.Remove:
				if v, ok := activeWatches[path]; ok {
					if v {
						logf("stopped watching (implicit) dir %q", path)
					}
					// We try to avoid sending remove events for directories, however
					// fsnotify send is two events per delete. One for the "DELETE_SELF",
					// and another for "DELETE,ISDIR". These flags aren't exposed by
					// fsnotify so after the file/directory has been removed there is
					// no way to tell if the path was a directory.
					// To prevent events for the second we just set the entry to false
					// w/o dropping it entirely.
					activeWatches[path] = false
					continue
				}
				eventCh <- Event{path, OpRemoved}
			case fsnotify.Chmod, fsnotify.Write, fsnotify.Create:
				var op Op
				if e.Op == fsnotify.Create {
					op = OpCreated
				} else {
					op = OpChanged
				}

				di, err := os.Stat(path)
				if err != nil {
					// TODO: continue instead?
					return fmt.Errorf("failed to stat %q: %v", path, err)
				}
				if !di.IsDir() {
					// We know for sure that this isn't a directory so we must remove
					// any entry from activeWatchers in case we have had a directory
					// with the same name earlier.
					delete(activeWatches, path)
					if !skipDir(path) {
						eventCh <- Event{path, op}
					}
					continue
				}
				files, err := populateWatchers(path)
				if err != nil {
					return fmt.Errorf("failed to walk %q: %v", path, err)
				}
				// If the directory was just created, we need to send file creation events
				// for all files created as well.
				if op == OpCreated && activeWatches[path] {
					for _, f := range files {
						eventCh <- Event{f, op}
					}
				}
			}
		}
		close(eventCh)
		return nil
	})

	return w, nil
}

func (w *fswatcher) Close() error {
	return w.mw.Close()
}

func (w *fswatcher) Events() chan Event {
	return w.eventCh
}

func (w *fswatcher) Errors() chan error {
	return w.errCh
}
