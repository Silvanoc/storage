package storage

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	drivers "github.com/containers/storage/drivers"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/ioutils"
	"github.com/containers/storage/pkg/stringid"
	"github.com/containers/storage/pkg/truncindex"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
)

const (
	tarSplitSuffix  = ".tar-split.gz"
	incompleteFlag  = "incomplete"
	compressionFlag = "diff-compression"
)

var (
	// ErrParentUnknown indicates that we didn't record the ID of the parent of the specified layer
	ErrParentUnknown = errors.New("parent of layer not known")
	// ErrLayerUnknown indicates that there was no layer with the specified name or ID
	ErrLayerUnknown = errors.New("layer not known")
)

// A Layer is a record of a copy-on-write layer that's stored by the lower
// level graph driver.
type Layer struct {
	// ID is either one which was specified at create-time, or a random
	// value which was generated by the library.
	ID string `json:"id"`

	// Names is an optional set of user-defined convenience values.  The
	// layer can be referred to by its ID or any of its names.  Names are
	// unique among layers.
	Names []string `json:"names,omitempty"`

	// Parent is the ID of a layer from which this layer inherits data.
	Parent string `json:"parent,omitempty"`

	// Metadata is data we keep for the convenience of the caller.  It is not
	// expected to be large, since it is kept in memory.
	Metadata string `json:"metadata,omitempty"`

	// MountLabel is an SELinux label which should be used when attempting to mount
	// the layer.
	MountLabel string `json:"mountlabel,omitempty"`

	// MountPoint is the path where the layer is mounted, or where it was most
	// recently mounted.  This can change between subsequent Unmount() and
	// Mount() calls, so the caller should consult this value after Mount()
	// succeeds to find the location of the container's root filesystem.
	MountPoint string `json:"-"`

	// MountCount is used as a reference count for the container's layer being
	// mounted at the mount point.
	MountCount int `json:"-"`

	Flags map[string]interface{} `json:"flags,omitempty"`
}

type layerMountPoint struct {
	ID         string `json:"id"`
	MountPoint string `json:"path"`
	MountCount int    `json:"count"`
}

// LayerStore wraps a graph driver, adding the ability to refer to layers by
// name, and keeping track of parent-child relationships, along with a list of
// all known layers.
type LayerStore interface {
	FileBasedStore
	MetadataStore
	FlaggableStore

	// Create creates a new layer, optionally giving it a specified ID rather than
	// a randomly-generated one, either inheriting data from another specified
	// layer or the empty base layer.  The new layer can optionally be given names
	// and have an SELinux label specified for use when mounting it.  Some
	// underlying drivers can accept a "size" option.  At this time, most
	// underlying drivers do not themselves distinguish between writeable
	// and read-only layers.
	Create(id, parent string, names []string, mountLabel string, options map[string]string, writeable bool) (*Layer, error)

	// CreateWithFlags combines the functions of Create and SetFlag.
	CreateWithFlags(id, parent string, names []string, mountLabel string, options map[string]string, writeable bool, flags map[string]interface{}) (layer *Layer, err error)

	// Put combines the functions of CreateWithFlags and ApplyDiff.
	Put(id, parent string, names []string, mountLabel string, options map[string]string, writeable bool, flags map[string]interface{}, diff archive.Reader) (*Layer, int64, error)

	// Exists checks if a layer with the specified name or ID is known.
	Exists(id string) bool

	// Get retrieves information about a layer given an ID or name.
	Get(id string) (*Layer, error)

	// SetNames replaces the list of names associated with a layer with the
	// supplied values.
	SetNames(id string, names []string) error

	// Status returns an slice of key-value pairs, suitable for human consumption,
	// relaying whatever status information the underlying driver can share.
	Status() ([][2]string, error)

	// Delete deletes a layer with the specified name or ID.
	Delete(id string) error

	// Wipe deletes all layers.
	Wipe() error

	// Mount mounts a layer for use.  If the specified layer is the parent of other
	// layers, it should not be written to.  An SELinux label to be applied to the
	// mount can be specified to override the one configured for the layer.
	Mount(id, mountLabel string) (string, error)

	// Unmount unmounts a layer when it is no longer in use.
	Unmount(id string) error

	// Changes returns a slice of Change structures, which contain a pathname
	// (Path) and a description of what sort of change (Kind) was made by the
	// layer (either ChangeModify, ChangeAdd, or ChangeDelete), relative to a
	// specified layer.  By default, the layer's parent is used as a reference.
	Changes(from, to string) ([]archive.Change, error)

	// Diff produces a tarstream which can be applied to a layer with the contents
	// of the first layer to produce a layer with the contents of the second layer.
	// By default, the parent of the second layer is used as the first
	// layer, so it need not be specified.
	Diff(from, to string) (io.ReadCloser, error)

	// DiffSize produces an estimate of the length of the tarstream which would be
	// produced by Diff.
	DiffSize(from, to string) (int64, error)

	// ApplyDiff reads a tarstream which was created by a previous call to Diff and
	// applies its changes to a specified layer.
	ApplyDiff(to string, diff archive.Reader) (int64, error)

	// Lookup attempts to translate a name to an ID.  Most methods do this
	// implicitly.
	Lookup(name string) (string, error)

	// Layers returns a slice of the known layers.
	Layers() ([]Layer, error)
}

type layerStore struct {
	lockfile Locker
	rundir   string
	driver   drivers.Driver
	layerdir string
	layers   []*Layer
	idindex  *truncindex.TruncIndex
	byid     map[string]*Layer
	byname   map[string]*Layer
	bymount  map[string]*Layer
}

func (r *layerStore) Layers() ([]Layer, error) {
	layers := make([]Layer, len(r.layers))
	for i := range r.layers {
		layers[i] = *(r.layers[i])
	}
	return layers, nil
}

func (r *layerStore) mountspath() string {
	return filepath.Join(r.rundir, "mountpoints.json")
}

func (r *layerStore) layerspath() string {
	return filepath.Join(r.layerdir, "layers.json")
}

func (r *layerStore) Load() error {
	needSave := false
	rpath := r.layerspath()
	data, err := ioutil.ReadFile(rpath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	layers := []*Layer{}
	idlist := []string{}
	ids := make(map[string]*Layer)
	names := make(map[string]*Layer)
	mounts := make(map[string]*Layer)
	parents := make(map[string][]*Layer)
	if err = json.Unmarshal(data, &layers); len(data) == 0 || err == nil {
		for n, layer := range layers {
			ids[layer.ID] = layers[n]
			idlist = append(idlist, layer.ID)
			for _, name := range layer.Names {
				if conflict, ok := names[name]; ok {
					r.removeName(conflict, name)
					needSave = true
				}
				names[name] = layers[n]
			}
			if pslice, ok := parents[layer.Parent]; ok {
				parents[layer.Parent] = append(pslice, layers[n])
			} else {
				parents[layer.Parent] = []*Layer{layers[n]}
			}
		}
	}
	mpath := r.mountspath()
	data, err = ioutil.ReadFile(mpath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	layerMounts := []layerMountPoint{}
	if err = json.Unmarshal(data, &layerMounts); len(data) == 0 || err == nil {
		for _, mount := range layerMounts {
			if mount.MountPoint != "" {
				if layer, ok := ids[mount.ID]; ok {
					mounts[mount.MountPoint] = layer
					layer.MountPoint = mount.MountPoint
					layer.MountCount = mount.MountCount
				}
			}
		}
	}
	r.layers = layers
	r.idindex = truncindex.NewTruncIndex(idlist)
	r.byid = ids
	r.byname = names
	r.bymount = mounts
	err = nil
	// Last step: try to remove anything that a previous user of this
	// storage area marked for deletion but didn't manage to actually
	// delete.
	for _, layer := range r.layers {
		if cleanup, ok := layer.Flags[incompleteFlag]; ok {
			if b, ok := cleanup.(bool); ok && b {
				err = r.Delete(layer.ID)
				if err != nil {
					break
				}
				needSave = true
			}
		}
	}
	if needSave {
		return r.Save()
	}
	return err
}

func (r *layerStore) Save() error {
	rpath := r.layerspath()
	if err := os.MkdirAll(filepath.Dir(rpath), 0700); err != nil {
		return err
	}
	jldata, err := json.Marshal(&r.layers)
	if err != nil {
		return err
	}
	mpath := r.mountspath()
	if err := os.MkdirAll(filepath.Dir(mpath), 0700); err != nil {
		return err
	}
	mounts := []layerMountPoint{}
	for _, layer := range r.layers {
		if layer.MountPoint != "" && layer.MountCount > 0 {
			mounts = append(mounts, layerMountPoint{
				ID:         layer.ID,
				MountPoint: layer.MountPoint,
				MountCount: layer.MountCount,
			})
		}
	}
	jmdata, err := json.Marshal(&mounts)
	if err != nil {
		return err
	}
	if err := ioutils.AtomicWriteFile(rpath, jldata, 0600); err != nil {
		return err
	}
	defer r.Touch()
	return ioutils.AtomicWriteFile(mpath, jmdata, 0600)
}

func newLayerStore(rundir string, layerdir string, driver drivers.Driver) (LayerStore, error) {
	if err := os.MkdirAll(rundir, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(layerdir, 0700); err != nil {
		return nil, err
	}
	lockfile, err := GetLockfile(filepath.Join(layerdir, "layers.lock"))
	if err != nil {
		return nil, err
	}
	lockfile.Lock()
	defer lockfile.Unlock()
	rlstore := layerStore{
		lockfile: lockfile,
		driver:   driver,
		rundir:   rundir,
		layerdir: layerdir,
		byid:     make(map[string]*Layer),
		bymount:  make(map[string]*Layer),
		byname:   make(map[string]*Layer),
	}
	if err := rlstore.Load(); err != nil {
		return nil, err
	}
	return &rlstore, nil
}

func (r *layerStore) lookup(id string) (*Layer, bool) {
	if layer, ok := r.byid[id]; ok {
		return layer, ok
	} else if layer, ok := r.byname[id]; ok {
		return layer, ok
	} else if longid, err := r.idindex.Get(id); err == nil {
		layer, ok := r.byid[longid]
		return layer, ok
	}
	return nil, false
}

func (r *layerStore) ClearFlag(id string, flag string) error {
	layer, ok := r.lookup(id)
	if !ok {
		return ErrLayerUnknown
	}
	delete(layer.Flags, flag)
	return r.Save()
}

func (r *layerStore) SetFlag(id string, flag string, value interface{}) error {
	layer, ok := r.lookup(id)
	if !ok {
		return ErrLayerUnknown
	}
	layer.Flags[flag] = value
	return r.Save()
}

func (r *layerStore) Status() ([][2]string, error) {
	return r.driver.Status(), nil
}

func (r *layerStore) Put(id, parent string, names []string, mountLabel string, options map[string]string, writeable bool, flags map[string]interface{}, diff archive.Reader) (layer *Layer, size int64, err error) {
	size = -1
	if err := os.MkdirAll(r.rundir, 0700); err != nil {
		return nil, -1, err
	}
	if err := os.MkdirAll(r.layerdir, 0700); err != nil {
		return nil, -1, err
	}
	if parent != "" {
		if parentLayer, ok := r.lookup(parent); ok {
			parent = parentLayer.ID
		}
	}
	if id == "" {
		id = stringid.GenerateRandomID()
		_, idInUse := r.byid[id]
		for idInUse {
			id = stringid.GenerateRandomID()
			_, idInUse = r.byid[id]
		}
	}
	if _, idInUse := r.byid[id]; idInUse {
		return nil, -1, ErrDuplicateID
	}
	for _, name := range names {
		if _, nameInUse := r.byname[name]; nameInUse {
			return nil, -1, ErrDuplicateName
		}
	}
	if writeable {
		err = r.driver.CreateReadWrite(id, parent, mountLabel, options)
	} else {
		err = r.driver.Create(id, parent, mountLabel, options)
	}
	if err == nil {
		layer = &Layer{
			ID:         id,
			Parent:     parent,
			Names:      names,
			MountLabel: mountLabel,
			Flags:      make(map[string]interface{}),
		}
		r.layers = append(r.layers, layer)
		r.idindex.Add(id)
		r.byid[id] = layer
		for _, name := range names {
			r.byname[name] = layer
		}
		for flag, value := range flags {
			layer.Flags[flag] = value
		}
		if diff != nil {
			layer.Flags[incompleteFlag] = true
			err = r.Save()
			if err != nil {
				// We don't have a record of this layer, but at least
				// try to clean it up underneath us.
				r.driver.Remove(id)
				return nil, -1, err
			}
			size, err = r.ApplyDiff(layer.ID, diff)
			if err != nil {
				if r.Delete(layer.ID) != nil {
					// Either a driver error or an error saving.
					// We now have a layer that's been marked for
					// deletion but which we failed to remove.
				}
				return nil, -1, err
			}
			delete(layer.Flags, incompleteFlag)
		}
		err = r.Save()
		if err != nil {
			// We don't have a record of this layer, but at least
			// try to clean it up underneath us.
			r.driver.Remove(id)
			return nil, -1, err
		}
	}
	return layer, size, err
}

func (r *layerStore) CreateWithFlags(id, parent string, names []string, mountLabel string, options map[string]string, writeable bool, flags map[string]interface{}) (layer *Layer, err error) {
	layer, _, err = r.Put(id, parent, names, mountLabel, options, writeable, flags, nil)
	return layer, err
}

func (r *layerStore) Create(id, parent string, names []string, mountLabel string, options map[string]string, writeable bool) (layer *Layer, err error) {
	return r.CreateWithFlags(id, parent, names, mountLabel, options, writeable, nil)
}

func (r *layerStore) Mount(id, mountLabel string) (string, error) {
	layer, ok := r.lookup(id)
	if !ok {
		return "", ErrLayerUnknown
	}
	if layer.MountCount > 0 {
		layer.MountCount++
		return layer.MountPoint, r.Save()
	}
	if mountLabel == "" {
		mountLabel = layer.MountLabel
	}
	mountpoint, err := r.driver.Get(id, mountLabel)
	if mountpoint != "" && err == nil {
		if layer.MountPoint != "" {
			delete(r.bymount, layer.MountPoint)
		}
		layer.MountPoint = filepath.Clean(mountpoint)
		layer.MountCount++
		r.bymount[layer.MountPoint] = layer
		err = r.Save()
	}
	return mountpoint, err
}

func (r *layerStore) Unmount(id string) error {
	layer, ok := r.lookup(id)
	if !ok {
		layerByMount, ok := r.bymount[filepath.Clean(id)]
		if !ok {
			return ErrLayerUnknown
		}
		layer = layerByMount
	}
	if layer.MountCount > 1 {
		layer.MountCount--
		return r.Save()
	}
	err := r.driver.Put(id)
	if err == nil || os.IsNotExist(err) {
		if layer.MountPoint != "" {
			delete(r.bymount, layer.MountPoint)
		}
		layer.MountCount--
		layer.MountPoint = ""
		err = r.Save()
	}
	return err
}

func (r *layerStore) removeName(layer *Layer, name string) {
	layer.Names = stringSliceWithoutValue(layer.Names, name)
}

func (r *layerStore) SetNames(id string, names []string) error {
	if layer, ok := r.lookup(id); ok {
		for _, name := range layer.Names {
			delete(r.byname, name)
		}
		for _, name := range names {
			if otherLayer, ok := r.byname[name]; ok {
				r.removeName(otherLayer, name)
			}
			r.byname[name] = layer
		}
		layer.Names = names
		return r.Save()
	}
	return ErrLayerUnknown
}

func (r *layerStore) Metadata(id string) (string, error) {
	if layer, ok := r.lookup(id); ok {
		return layer.Metadata, nil
	}
	return "", ErrLayerUnknown
}

func (r *layerStore) SetMetadata(id, metadata string) error {
	if layer, ok := r.lookup(id); ok {
		layer.Metadata = metadata
		return r.Save()
	}
	return ErrLayerUnknown
}

func (r *layerStore) tspath(id string) string {
	return filepath.Join(r.layerdir, id+tarSplitSuffix)
}

func (r *layerStore) Delete(id string) error {
	layer, ok := r.lookup(id)
	if !ok {
		return ErrLayerUnknown
	}
	id = layer.ID
	for layer.MountCount > 0 {
		if err := r.Unmount(id); err != nil {
			return err
		}
	}
	err := r.driver.Remove(id)
	if err == nil {
		os.Remove(r.tspath(id))
		delete(r.byid, id)
		r.idindex.Delete(id)
		if layer.MountPoint != "" {
			delete(r.bymount, layer.MountPoint)
		}
		newLayers := []*Layer{}
		for _, candidate := range r.layers {
			if candidate.ID != id {
				newLayers = append(newLayers, candidate)
			}
		}
		r.layers = newLayers
		if err = r.Save(); err != nil {
			return err
		}
	}
	return err
}

func (r *layerStore) Lookup(name string) (id string, err error) {
	if layer, ok := r.lookup(name); ok {
		return layer.ID, nil
	}
	return "", ErrLayerUnknown
}

func (r *layerStore) Exists(id string) bool {
	_, ok := r.lookup(id)
	return ok
}

func (r *layerStore) Get(id string) (*Layer, error) {
	if layer, ok := r.lookup(id); ok {
		return layer, nil
	}
	return nil, ErrLayerUnknown
}

func (r *layerStore) Wipe() error {
	ids := []string{}
	for id := range r.byid {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := r.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

func (r *layerStore) findParentAndLayer(from, to string) (fromID string, toID string, toLayer *Layer, err error) {
	var ok bool
	var fromLayer *Layer
	toLayer, ok = r.lookup(to)
	if !ok {
		return "", "", nil, ErrLayerUnknown
	}
	to = toLayer.ID
	if from == "" {
		from = toLayer.Parent
	}
	if from != "" {
		fromLayer, ok = r.lookup(from)
		if ok {
			from = fromLayer.ID
		} else {
			fromLayer, ok = r.lookup(toLayer.Parent)
			if ok {
				from = fromLayer.ID
			}
		}
	}
	return from, to, toLayer, nil
}

func (r *layerStore) Changes(from, to string) ([]archive.Change, error) {
	from, to, _, err := r.findParentAndLayer(from, to)
	if err != nil {
		return nil, ErrLayerUnknown
	}
	return r.driver.Changes(to, from)
}

type simpleGetCloser struct {
	r    *layerStore
	path string
	id   string
}

func (s *simpleGetCloser) Get(path string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(s.path, path))
}

func (s *simpleGetCloser) Close() error {
	return s.r.Unmount(s.id)
}

func (r *layerStore) newFileGetter(id string) (drivers.FileGetCloser, error) {
	if getter, ok := r.driver.(drivers.DiffGetterDriver); ok {
		return getter.DiffGetter(id)
	}
	path, err := r.Mount(id, "")
	if err != nil {
		return nil, err
	}
	return &simpleGetCloser{
		r:    r,
		path: path,
		id:   id,
	}, nil
}

func (r *layerStore) Diff(from, to string) (io.ReadCloser, error) {
	var metadata storage.Unpacker

	from, to, toLayer, err := r.findParentAndLayer(from, to)
	if err != nil {
		return nil, ErrLayerUnknown
	}
	compression := archive.Uncompressed
	if cflag, ok := toLayer.Flags[compressionFlag]; ok {
		if ctype, ok := cflag.(float64); ok {
			compression = archive.Compression(ctype)
		} else if ctype, ok := cflag.(archive.Compression); ok {
			compression = archive.Compression(ctype)
		}
	}
	if from != toLayer.Parent {
		diff, err := r.driver.Diff(to, from)
		if err == nil && (compression != archive.Uncompressed) {
			preader, pwriter := io.Pipe()
			compressor, err := archive.CompressStream(pwriter, compression)
			if err != nil {
				diff.Close()
				pwriter.Close()
				return nil, err
			}
			go func() {
				io.Copy(compressor, diff)
				diff.Close()
				compressor.Close()
				pwriter.Close()
			}()
			diff = preader
		}
		return diff, err
	}

	tsfile, err := os.Open(r.tspath(to))
	if err != nil {
		if os.IsNotExist(err) {
			return r.driver.Diff(to, from)
		}
		return nil, err
	}
	defer tsfile.Close()

	decompressor, err := gzip.NewReader(tsfile)
	if err != nil {
		return nil, err
	}
	defer decompressor.Close()

	tsbytes, err := ioutil.ReadAll(decompressor)
	if err != nil {
		return nil, err
	}

	metadata = storage.NewJSONUnpacker(bytes.NewBuffer(tsbytes))

	fgetter, err := r.newFileGetter(to)
	if err != nil {
		return nil, err
	}

	var stream io.ReadCloser
	if compression != archive.Uncompressed {
		preader, pwriter := io.Pipe()
		compressor, err := archive.CompressStream(pwriter, compression)
		if err != nil {
			fgetter.Close()
			pwriter.Close()
			preader.Close()
			return nil, err
		}
		go func() {
			asm.WriteOutputTarStream(fgetter, metadata, compressor)
			compressor.Close()
			pwriter.Close()
		}()
		stream = preader
	} else {
		stream = asm.NewOutputTarStream(fgetter, metadata)
	}
	return ioutils.NewReadCloserWrapper(stream, func() error {
		err1 := stream.Close()
		err2 := fgetter.Close()
		if err2 == nil {
			return err1
		}
		return err2
	}), nil
}

func (r *layerStore) DiffSize(from, to string) (size int64, err error) {
	from, to, _, err = r.findParentAndLayer(from, to)
	if err != nil {
		return -1, ErrLayerUnknown
	}
	return r.driver.DiffSize(to, from)
}

func (r *layerStore) ApplyDiff(to string, diff archive.Reader) (size int64, err error) {
	layer, ok := r.lookup(to)
	if !ok {
		return -1, ErrLayerUnknown
	}

	header := make([]byte, 10240)
	n, err := diff.Read(header)
	if err != nil && err != io.EOF {
		return -1, err
	}

	compression := archive.DetectCompression(header[:n])
	defragmented := io.MultiReader(bytes.NewBuffer(header[:n]), diff)

	tsdata := bytes.Buffer{}
	compressor, err := gzip.NewWriterLevel(&tsdata, gzip.BestSpeed)
	if err != nil {
		compressor = gzip.NewWriter(&tsdata)
	}
	metadata := storage.NewJSONPacker(compressor)
	decompressed, err := archive.DecompressStream(defragmented)
	if err != nil {
		return -1, err
	}
	payload, err := asm.NewInputTarStream(decompressed, metadata, storage.NewDiscardFilePutter())
	if err != nil {
		return -1, err
	}
	size, err = r.driver.ApplyDiff(layer.ID, layer.Parent, payload)
	compressor.Close()
	if err == nil {
		if err := os.MkdirAll(filepath.Dir(r.tspath(layer.ID)), 0700); err != nil {
			return -1, err
		}
		if err := ioutils.AtomicWriteFile(r.tspath(layer.ID), tsdata.Bytes(), 0600); err != nil {
			return -1, err
		}
	}

	if compression != archive.Uncompressed {
		layer.Flags[compressionFlag] = compression
	} else {
		delete(layer.Flags, compressionFlag)
	}

	return size, err
}

func (r *layerStore) Lock() {
	r.lockfile.Lock()
}

func (r *layerStore) Unlock() {
	r.lockfile.Unlock()
}

func (r *layerStore) Touch() error {
	return r.lockfile.Touch()
}

func (r *layerStore) Modified() (bool, error) {
	return r.lockfile.Modified()
}

func (r *layerStore) IsReadWrite() bool {
	return r.lockfile.IsReadWrite()
}

func (r *layerStore) TouchedSince(when time.Time) bool {
	return r.lockfile.TouchedSince(when)
}
