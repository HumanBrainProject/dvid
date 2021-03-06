package filelog

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"
	"github.com/janelia-flyem/go/semver"
	"github.com/janelia-flyem/go/uuid"
)

func init() {
	ver, err := semver.Make("0.1.0")
	if err != nil {
		dvid.Errorf("Unable to make semver in filelog: %v\n", err)
	}
	e := Engine{"filelog", "File-based log", ver}
	storage.RegisterEngine(e)
}

// --- Engine Implementation ------

type Engine struct {
	name   string
	desc   string
	semver semver.Version
}

func (e Engine) GetName() string {
	return e.name
}

func (e Engine) GetDescription() string {
	return e.desc
}

func (e Engine) IsDistributed() bool {
	return false
}

func (e Engine) GetSemVer() semver.Version {
	return e.semver
}

func (e Engine) String() string {
	return fmt.Sprintf("%s [%s]", e.name, e.semver)
}

// NewStore returns file-based log. The passed Config must contain "path" setting.
func (e Engine) NewStore(config dvid.StoreConfig) (dvid.Store, bool, error) {
	return e.newWriteLogs(config)
}

func parseConfig(config dvid.StoreConfig) (path string, testing bool, err error) {
	c := config.GetAll()

	v, found := c["path"]
	if !found {
		err = fmt.Errorf("%q must be specified for log configuration", "path")
		return
	}
	var ok bool
	path, ok = v.(string)
	if !ok {
		err = fmt.Errorf("%q setting must be a string (%v)", "path", v)
		return
	}
	v, found = c["testing"]
	if found {
		testing, ok = v.(bool)
		if !ok {
			err = fmt.Errorf("%q setting must be a bool (%v)", "testing", v)
			return
		}
	}
	if testing {
		path = filepath.Join(os.TempDir(), path)
	}
	return
}

// newWriteLogs returns a file-based append-only log backend, creating a log
// at the path if it doesn't already exist.
func (e Engine) newWriteLogs(config dvid.StoreConfig) (*writeLogs, bool, error) {
	path, _, err := parseConfig(config)
	if err != nil {
		return nil, false, err
	}

	var created bool
	if _, err := os.Stat(path); os.IsNotExist(err) {
		dvid.Infof("Log not already at path (%s). Creating ...\n", path)
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, false, err
		}
		created = true
	} else {
		dvid.Infof("Found log at %s (err = %v)\n", path, err)
	}

	// opt, err := getOptions(config.Config)
	// if err != nil {
	// 	return nil, false, err
	// }

	log := &writeLogs{
		path:   path,
		config: config,
		files:  make(map[string]*flog),
	}
	return log, created, nil
}

type writeLogs struct {
	path   string
	config dvid.StoreConfig
	files  map[string]*flog // key = data + version UUID
}

func (wlogs *writeLogs) getLogFile(dataID, version dvid.UUID) (fl *flog, err error) {
	k := string(dataID + "-" + version)
	var found bool
	fl, found = wlogs.files[k]
	if !found {
		filename := filepath.Join(wlogs.path, k)
		var f *os.File
		f, err = os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_SYNC, 0755)
		if err != nil {
			return
		}
		fl = &flog{File: f}
		wlogs.files[k] = fl
	}
	return
}

func (wlogs *writeLogs) Append(entryType uint16, dataID, version dvid.UUID, p []byte) error {
	fl, err := wlogs.getLogFile(dataID, version)
	if err != nil {
		return fmt.Errorf("append log %q: %v", wlogs, err)
	}
	fl.Lock()
	if err = fl.writeHeader(entryType, p); err != nil {
		fl.Unlock()
		return fmt.Errorf("bad write of log header to data %s, uuid %s: %v\n", dataID, version, err)
	}
	_, err = fl.Write(p)
	fl.Unlock()
	if err != nil {
		err = fmt.Errorf("append log %q: %v", wlogs, err)
	}
	return err
}

func (wlogs *writeLogs) Close() {
	for _, flog := range wlogs.files {
		flog.Lock()
		err := flog.Close()
		if err != nil {
			dvid.Errorf("closing log file %q: %v\n", flog.Name(), err)
		}
		flog.Unlock()
	}
}

func (wlogs *writeLogs) String() string {
	return fmt.Sprintf("write logs @ path %q", wlogs.path)
}

// Equal returns true if the write log path matches the given store configuration.
func (wlogs *writeLogs) Equal(config dvid.StoreConfig) bool {
	path, _, err := parseConfig(config)
	if err != nil {
		return false
	}
	return path == wlogs.path
}

type flog struct {
	*os.File
	sync.RWMutex
}

func (f *flog) writeHeader(entryType uint16, data []byte) error {
	buf := make([]byte, 6)
	binary.LittleEndian.PutUint16(buf[:2], entryType)
	size := uint32(len(data))
	binary.LittleEndian.PutUint32(buf[2:], size)
	_, err := f.Write(buf)
	return err
}

func (f *flog) readHeader() (entryType uint16, size uint32, err error) {
	buf := make([]byte, 6)
	_, err = io.ReadFull(f, buf)
	if err != nil {
		return
	}
	entryType = binary.LittleEndian.Uint16(buf[0:2])
	size = binary.LittleEndian.Uint32(buf[2:])
	return
}

// ---- TestableEngine interface implementation -------

// AddTestConfig sets the filelog as the default append-only log.  If another engine is already
// set for the append-only log, it returns an error since only one append-only log backend should
// be tested via tags.
func (e Engine) AddTestConfig(backend *storage.Backend) error {
	if backend.DefaultLog != "" {
		return fmt.Errorf("filelog can't be testable log.  DefaultLog already set to %s", backend.DefaultLog)
	}
	alias := storage.Alias("filelog")
	backend.DefaultLog = alias
	if backend.Stores == nil {
		backend.Stores = make(map[storage.Alias]dvid.StoreConfig)
	}
	tc := map[string]interface{}{
		"path":    fmt.Sprintf("dvid-test-filelog-%x", uuid.NewV4().Bytes()),
		"testing": true,
	}
	var c dvid.Config
	c.SetAll(tc)
	backend.Stores[alias] = dvid.StoreConfig{Config: c, Engine: "filelog"}
	return nil
}

// Delete implements the TestableEngine interface by providing a way to dispose
// of the testable filelog.
func (e Engine) Delete(config dvid.StoreConfig) error {
	path, _, err := parseConfig(config)
	if err != nil {
		return err
	}

	// Delete the directory if it exists
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("Can't delete old append-only log directory %q: %v", path, err)
		}
	}
	return nil
}
