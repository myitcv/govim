//go:build darwin
// +build darwin

package fswatcher

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsevents"
	"gopkg.in/tomb.v2"
)

const (
	fRemoved = fsevents.ItemRemoved | fsevents.ItemRenamed
	fChanged = fsevents.ItemModified | fsevents.ItemChangeOwner
	fCreated = fsevents.ItemCreated
)

type fswatcher struct {
	eventCh chan Event
	es      *fsevents.EventStream
}

func New(root string, skipDir WatchFilterFn, logf LogFn, tomb *tomb.Tomb) (*FSWatcher, error) {
	dirpath := filepath.Dir(root)
	dev, err := fsevents.DeviceForPath(dirpath)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve device for path %v: %v", dirpath, err)
	}

	es := &fsevents.EventStream{
		Paths:   []string{dirpath},
		Latency: 200 * time.Millisecond,
		Device:  dev,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}

	es.Start()

	// fsevents returns paths relative to device root so we need
	// to figure out the actual mount point
	mountPoint, err := filepath.Abs(dirpath)
	if err != nil {
		log.Fatal(err)
	}

	for mountPoint != string(os.PathSeparator) {
		parent := filepath.Dir(mountPoint)
		pDev, err := fsevents.DeviceForPath(parent)
		if err != nil {
			log.Fatal(err)
		}
		if pDev != dev {
			break
		}
		mountPoint = parent
	}

	eventCh := make(chan Event)
	tomb.Go(func() error {
		for {
			events, ok := <-es.Events
			if !ok {
				break
			}
			for i := range events {
				event := events[i]
				path := filepath.Join(mountPoint, event.Path)

				if skipDir(path) {
					continue
				}

				// Darwin might include both "created" and "changed" in the same event
				// so ordering matters below. The "created" case should be checked
				// before "changed" to get a behavior that is more consistent with other
				// os_other.go.
				switch {
				case event.Flags&fRemoved > 0:
					eventCh <- Event{path, OpRemoved}
				case event.Flags&fCreated > 0:
					eventCh <- Event{path, OpCreated}
				case event.Flags&fChanged > 0:
					eventCh <- Event{path, OpChanged}
				}
			}
		}
		close(eventCh)
		return nil
	})

	return &FSWatcher{&fswatcher{eventCh, es}}, nil
}

func (w *fswatcher) Close() error {
	w.es.Stop()
	return nil
}
func (w *fswatcher) Events() chan Event { return w.eventCh }
func (w *fswatcher) Errors() chan error { return make(chan error) }
