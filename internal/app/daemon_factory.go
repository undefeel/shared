package app

import (
	"log"
	"sharedir/internal/folder"
)

type folderFactory func(root, state, device string, logger *log.Logger) (*folder.Folder, error)

// newWithFolderFactory is private: the factory must install probes before
// returning, and is immutable once the daemon starts.
func newWithFolderFactory(home string, logger *log.Logger, factory folderFactory) (*Daemon, error) {
	d, err := New(home, logger)
	if err == nil {
		d.folderFactory = factory
	}
	return d, err
}
func (d *Daemon) openFolder(root, state string) (*folder.Folder, error) {
	factory := d.folderFactory
	if factory == nil {
		factory = folder.Open
	}
	return factory(root, state, d.ID.Short(), d.Log)
}
