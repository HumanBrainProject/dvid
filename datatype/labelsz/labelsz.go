/*
	Package labelsz supports ranking labels by # annotations of each type.
*/
package labelsz

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/annotation"
	"github.com/janelia-flyem/dvid/datatype/roi"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

const (
	Version  = "0.1"
	RepoURL  = "github.com/janelia-flyem/dvid/datatype/labelsz"
	TypeName = "labelsz"
)

const HelpMessage = `
API for labelsz data type (github.com/janelia-flyem/dvid/datatype/labelsz)
=======================================================================================

Command-line:

$ dvid repo <UUID> new labelsz <data name> <settings...>

	Adds newly named data of the 'type name' to repo with specified UUID.

	Example:

	$ dvid repo 3f8c new labelsz labelrankings

    Arguments:

    UUID           Hexidecimal string with enough characters to uniquely identify a version node.
    data name      Name of data to create, e.g., "labelrankings"
    settings       Configuration settings in "key=value" format separated by spaces.

    Configuration Settings (case-insensitive keys)

    ROI            Value must be in "<roiname>,<uuid>" format where <roiname> is the name of the
				   static ROI that defines the extent of tracking and <uuid> is the immutable
				   version used for this labelsz.
	
    ------------------

HTTP API (Level 2 REST):

GET  <api URL>/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  <api URL>/node/<UUID>/<data name>/info
POST <api URL>/node/<UUID>/<data name>/info

    Retrieves or puts DVID-specific data properties for this labelsz data instance.

    Example: 

    GET <api URL>/node/3f8c/labelrankings/info

    Returns JSON with configuration settings.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of labelsz data.


POST <api URL>/node/<UUID>/<data name>/sync

    Establishes data instances for which the label sizes are computed.  Expects JSON to be POSTed
    with the following format:

    { "sync": "synapses" }

    The "sync" property should be followed by a comma-delimited list of data instances that MUST
    already exist.  After this sync request, the labelsz data are computed for the first time
	and then kept in sync thereafter.  It is not allowed to change syncs.  You can, however,
	create a new labelsz data instance and sync it as required.

    The labelsz data type only accepts syncs to annotation data instances.


GET <api URL>/node/<UUID>/<data name>/count/<label>/<index type>

	Returns the count of the given annotation element type for the given label.
	The index type may be any annotation element type ("PostSyn", "PreSyn", "Gap", "Note"),
	the catch-all for synapses "AllSyn", or the number of voxels "Voxels".

	For synapse indexing, the labelsz data instance must be synced with an annotations instance.
	(future) For # voxel indexing, the labelsz data instance must be synced with a labelvol instance.

	Example:

	GET <api URL>/node/3f8c/labelrankings/size/21847/PreSyn 

	Returns:

	{ "Label": 21847,  "PreSyn": 81 }

Note: For the following URL endpoints that return and accept POSTed JSON values, see the JSON format
at end of this documentation.

GET <api URL>/node/<UUID>/<data name>/top/<N>/<index type>

	Returns a list of the top N labels with respect to number of the specified index type.
	The index type may be any annotation element type ("PostSyn", "PreSyn", "Gap", "Note"),
	the catch-all for synapses "AllSyn", or the number of voxels "Voxels".

	For synapse indexing, the labelsz data instance must be synced with an annotations instance.
	(future) For # voxel indexing, the labelsz data instance must be synced with a labelvol instance.

	Example:

	GET <api URL>/node/3f8c/labelrankings/top/3/PreSyn 

	Returns:

	[ { "Label": 188,  "PreSyn": 81 }, { "Label": 23, "PreSyn": 65 }, { "Label": 8137, "PreSyn": 58 } ]

GET <api URL>/node/<UUID>/<data name>/threshold/<T>/<index type>[?<options>]

	Returns a list of up to 10,000 labels per request that have # given element types >= T.
	The "page" size is 10,000 labels so a call without any query string will return the 
	largest labels with # given element types >= T.  If there are more than 10,000 labels,
	you can access the next 10,000 by including "?offset=10001".

	The index type may be any annotation element type ("PostSyn", "PreSyn", "Gap", "Note"),
	the catch-all for synapses "AllSyn", or the number of voxels "Voxels".

	For synapse indexing, the labelsz data instance must be synced with an annotations instance.
	(future) For # voxel indexing, the labelsz data instance must be synced with a labelvol instance.

    GET Query-string Options:

    offset  The starting rank in the sorted list (in descending order) of labels with # given element types >= T.
    n       Number of labels to return.

	Example:

	GET <api URL>/node/3f8c/labelrankings/threshold/10/PreSyn?offset=10001&n=3

	Returns:

	[ { "Label": 188,  "PreSyn": 38 }, { "Label": 23, "PreSyn": 38 }, { "Label": 8137, "PreSyn": 37 } ]

	In the above example, the query returns the labels ranked #10,001 to #10,003 in the sorted list, in
	descending order of # PreSyn >= 10.
`

var (
	dtype *Type
)

const (
	MaxLabelsReturned = 10000 // Maximum number of labels returned in JSON
)

func init() {
	dtype = new(Type)
	dtype.Type = datastore.Type{
		Name:    TypeName,
		URL:     RepoURL,
		Version: Version,
		Requirements: &storage.Requirements{
			Batcher: true,
		},
	}

	// See doc for package on why channels are segregated instead of interleaved.
	// Data types must be registered with the datastore to be used.
	datastore.Register(dtype)

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Type{})
	gob.Register(&Data{})
}

// LabelSize is the count for a given label for some metric like # of PreSyn annotations.
type LabelSize struct {
	Label uint64
	Size  uint32
}

// LabelSizes is a sortable slice of LabelSize
type LabelSizes []LabelSize

// --- Sort interface

func (s LabelSizes) Len() int {
	return len(s)
}

func (s LabelSizes) Less(i, j int) bool {
	return s[i].Size < s[j].Size
}

func (s LabelSizes) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// NewData returns a pointer to labelsz data.
func NewData(uuid dvid.UUID, id dvid.InstanceID, name dvid.InstanceName, c dvid.Config) (*Data, error) {
	// See if we have a valid DataService ROI
	var roistr string
	roistr, found, err := c.GetString("ROI")
	if err != nil {
		return nil, err
	}
	if found {
		parts := strings.Split(roistr, ",")
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad ROI value (%q) expected %q", roistr, "<roiname>,<uuid>")
		}
	}

	// Initialize the Data for this data type
	basedata, err := datastore.NewDataService(dtype, uuid, id, name, c)
	if err != nil {
		return nil, err
	}
	data := &Data{
		Data: basedata,
		Properties: Properties{
			StaticROI: roistr,
		},
	}
	return data, nil
}

// --- Labelsz Datatype -----

type Type struct {
	datastore.Type
}

// --- TypeService interface ---

func (dtype *Type) NewDataService(uuid dvid.UUID, id dvid.InstanceID, name dvid.InstanceName, c dvid.Config) (datastore.DataService, error) {
	return NewData(uuid, id, name, c)
}

func (dtype *Type) Help() string {
	return HelpMessage
}

// Properties are additional properties for data beyond those in standard datastore.Data.
type Properties struct {
	// StaticROI is an optional static ROI specification of the form "<roiname>,<uuid>"
	// Note that it *cannot* mutate after the labelsz instance is created.
	StaticROI string
}

// Data instance of labelvol, label sparse volumes.
type Data struct {
	*datastore.Data
	Properties

	// Keep track of sync operations that could be updating the data.
	datastore.Updater

	// cache of immutable ROI on which this labelsz is filtered if any.
	iMutex     sync.Mutex
	iROI       *roi.Immutable
	roiChecked bool

	sync.RWMutex
}

func (d *Data) Equals(d2 *Data) bool {
	if !d.Data.Equals(d2.Data) {
		return false
	}
	return reflect.DeepEqual(d.Properties, d2.Properties)
}

func (d *Data) GetSyncedAnnotation() *annotation.Data {
	for dataUUID := range d.SyncedData() {
		source, err := annotation.GetByDataUUID(dataUUID)
		if err == nil {
			return source
		}
	}
	return nil
}

func (d *Data) inROI(e annotation.ElementPos) bool {
	if d.StaticROI == "" {
		return true // no ROI so ROI == everything
	}

	// Make sure we have immutable ROI if specified.
	d.iMutex.Lock()
	if !d.roiChecked {
		d.roiChecked = true
		iROI, err := roi.ImmutableBySpec(d.StaticROI)
		if err != nil {
			dvid.Errorf("could not load immutable ROI by spec %q: %v\n", d.StaticROI, err)
			d.iMutex.Unlock()
			return false
		}
		d.iROI = iROI
	}
	d.iMutex.Unlock()

	if d.iROI == nil {
		return false // ROI cannot be retrieved so use nothing; makes obvious failure since no ranks.
	}
	return d.iROI.VoxelWithin(e.Pos)
}

// GetCountElementType returns a count of the given ElementType for a given label.
func (d *Data) GetCountElementType(ctx *datastore.VersionedCtx, label uint64, i IndexType) (uint32, error) {
	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return 0, err
	}

	d.RLock()
	defer d.RUnlock()

	val, err := store.Get(ctx, NewTypeLabelTKey(i, label))
	if err != nil {
		return 0, err
	}
	if val == nil {
		return 0, nil
	}
	if len(val) != 4 {
		return 0, fmt.Errorf("bad size in value for index type %s, label %d: value has length %d", i, label, len(val))
	}
	count := binary.LittleEndian.Uint32(val)
	return count, nil
}

// GetTopElementType returns a sorted list of the top N labels that have the given ElementType.
func (d *Data) GetTopElementType(ctx *datastore.VersionedCtx, n int, i IndexType) (LabelSizes, error) {
	if n < 0 {
		return nil, fmt.Errorf("bad N (%d) in top request", n)
	}
	if n == 0 {
		return LabelSizes{}, nil
	}

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return nil, err
	}

	// Setup key range for iterating through keys of this ElementType.
	begTKey := NewTypeSizeLabelTKey(i, math.MaxUint32-1, 0)
	endTKey := NewTypeSizeLabelTKey(i, 0, math.MaxUint64)

	d.RLock()
	defer d.RUnlock()

	// Iterate through the first N kv then abort.
	shortCircuitErr := fmt.Errorf("Found data, aborting.")
	lsz := make(LabelSizes, n)
	rank := 0
	err = store.ProcessRange(ctx, begTKey, endTKey, nil, func(chunk *storage.Chunk) error {
		idxType, sz, label, err := DecodeTypeSizeLabelTKey(chunk.K)
		if err != nil {
			return err
		}
		if idxType != i {
			return fmt.Errorf("bad iteration of keys: expected index type %s, got %s", i, idxType)
		}
		lsz[rank] = LabelSize{Label: label, Size: sz}
		rank++
		if rank >= n {
			return shortCircuitErr
		}
		return nil
	})
	if err != shortCircuitErr && err != nil {
		return nil, err
	}
	return lsz[:rank], nil
}

// GetLabelsByThreshold returns a sorted list of labels that meet the given minSize threshold.
// We allow a maximum of MaxLabelsReturned returned labels and start with rank "offset".
func (d *Data) GetLabelsByThreshold(ctx *datastore.VersionedCtx, i IndexType, minSize uint32, offset, num int) (LabelSizes, error) {
	var nReturns int
	if num == 0 {
		nReturns = MaxLabelsReturned
	} else if num < 0 {
		return nil, fmt.Errorf("bad number of requested labels (%d)", num)
	} else {
		nReturns = num
	}

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return nil, err
	}

	// Setup key range for iterating through keys of this ElementType.
	begTKey := NewTypeSizeLabelTKey(i, math.MaxUint32-1, 0)
	endTKey := NewTypeSizeLabelTKey(i, 0, math.MaxUint64)

	d.RLock()
	defer d.RUnlock()

	// Iterate through sorted size list until we get what we need.
	shortCircuitErr := fmt.Errorf("Found data, aborting.")
	lsz := make(LabelSizes, nReturns)
	rank := 0
	saved := 0
	err = store.ProcessRange(ctx, begTKey, endTKey, nil, func(chunk *storage.Chunk) error {
		idxType, sz, label, err := DecodeTypeSizeLabelTKey(chunk.K)
		if err != nil {
			return err
		}
		if idxType != i {
			return fmt.Errorf("bad iteration of keys: expected index type %s, got %s", i, idxType)
		}
		if sz < minSize {
			return shortCircuitErr
		}
		if rank >= offset && rank < offset+nReturns {
			lsz[saved] = LabelSize{Label: label, Size: sz}
			saved++
			if saved == nReturns {
				return shortCircuitErr
			}
		}
		rank++
		return nil
	})
	if err != shortCircuitErr && err != nil {
		return nil, err
	}
	return lsz[:saved], nil
}

// GetByUUIDName returns a pointer to annotation data given a version (UUID) and data name.
func GetByUUIDName(uuid dvid.UUID, name dvid.InstanceName) (*Data, error) {
	source, err := datastore.GetDataByUUIDName(uuid, name)
	if err != nil {
		return nil, err
	}
	data, ok := source.(*Data)
	if !ok {
		return nil, fmt.Errorf("Instance '%s' is not a labelsz datatype!", name)
	}
	return data, nil
}

// --- datastore.DataService interface ---------

func (d *Data) Help() string {
	return HelpMessage
}

func (d *Data) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Base     *datastore.Data
		Extended Properties
	}{
		d.Data,
		d.Properties,
	})
}

func (d *Data) GobDecode(b []byte) error {
	buf := bytes.NewBuffer(b)
	dec := gob.NewDecoder(buf)
	if err := dec.Decode(&(d.Data)); err != nil {
		return err
	}
	if err := dec.Decode(&(d.Properties)); err != nil {
		return err
	}
	return nil
}

func (d *Data) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(d.Data); err != nil {
		return nil, err
	}
	if err := enc.Encode(d.Properties); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DoRPC acts as a switchboard for RPC commands.
func (d *Data) DoRPC(request datastore.Request, reply *datastore.Response) error {
	switch request.TypeCommand() {
	default:
		return fmt.Errorf("Unknown command.  Data type '%s' [%s] does not support '%s' command.",
			d.DataName(), d.TypeName(), request.TypeCommand())
	}
}

// ServeHTTP handles all incoming HTTP requests for this data.
func (d *Data) ServeHTTP(uuid dvid.UUID, ctx *datastore.VersionedCtx, w http.ResponseWriter, r *http.Request) {
	timedLog := dvid.NewTimeLog()

	// Get the action (GET, POST)
	action := strings.ToLower(r.Method)

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}

	// Handle POST on data -> setting of configuration
	if len(parts) == 3 && action == "put" {
		config, err := server.DecodeJSON(r)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		if err := d.ModifyConfig(config); err != nil {
			server.BadRequest(w, r, err)
			return
		}
		if err := datastore.SaveDataByUUID(uuid, d); err != nil {
			server.BadRequest(w, r, err)
			return
		}
		fmt.Fprintf(w, "Changed '%s' based on received configuration:\n%s\n", d.DataName(), config)
		return
	}

	if len(parts) < 4 {
		server.BadRequest(w, r, "Incomplete API request")
		return
	}

	// Process help and info.
	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, dtype.Help())

	case "info":
		jsonBytes, err := d.MarshalJSON()
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, string(jsonBytes))

	case "sync":
		if action != "post" {
			server.BadRequest(w, r, "Only POST allowed to sync endpoint")
			return
		}
		if err := datastore.SetSyncByJSON(d, uuid, r.Body); err != nil {
			server.BadRequest(w, r, err)
			return
		}

	case "count":
		if action != "get" {
			server.BadRequest(w, r, "Only GET action is available on 'count' endpoint.")
			return
		}
		if len(parts) < 6 {
			server.BadRequest(w, r, "Must include label and element type after 'count' endpoint.")
			return
		}
		label, err := strconv.ParseUint(parts[4], 10, 64)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		i := StringToIndexType(parts[5])
		if i == UnknownIndex {
			server.BadRequest(w, r, fmt.Errorf("unknown index type specified (%q)", parts[5]))
			return
		}
		count, err := d.GetCountElementType(ctx, label, i)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		w.Header().Set("Content-type", "application/json")
		jsonStr := fmt.Sprintf(`{"Label":%d,%q:%d}`, label, i, count)
		if _, err := w.Write([]byte(jsonStr)); err != nil {
			server.BadRequest(w, r, err)
			return
		}
		timedLog.Infof("HTTP %s: get count for label %d, index type %s: %s", r.Method, label, i, r.URL)

	case "top":
		if action != "get" {
			server.BadRequest(w, r, "Only GET action is available on 'top' endpoint.")
			return
		}
		if len(parts) < 6 {
			server.BadRequest(w, r, "Must include N and element type after 'top' endpoint.")
			return
		}
		n, err := strconv.ParseUint(parts[4], 10, 32)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		i := StringToIndexType(parts[5])
		if i == UnknownIndex {
			server.BadRequest(w, r, fmt.Errorf("unknown index type specified (%q)", parts[5]))
			return
		}
		labelSizes, err := d.GetTopElementType(ctx, int(n), i)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		w.Header().Set("Content-type", "application/json")
		jsonBytes, err := json.Marshal(labelSizes)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		if _, err := w.Write(jsonBytes); err != nil {
			server.BadRequest(w, r, err)
			return
		}
		timedLog.Infof("HTTP %s: get top %d labels for index type %s: %s", r.Method, n, i, r.URL)

	case "threshold":
		if action != "get" {
			server.BadRequest(w, r, "Only GET action is available on 'threshold' endpoint.")
			return
		}
		if len(parts) < 6 {
			server.BadRequest(w, r, "Must include threshold # and element type after 'threshold' endpoint.")
			return
		}
		t, err := strconv.ParseUint(parts[4], 10, 32)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		minSize := uint32(t)
		i := StringToIndexType(parts[5])
		if i == UnknownIndex {
			server.BadRequest(w, r, fmt.Errorf("unknown index type specified (%q)", parts[5]))
			return
		}

		queryStrings := r.URL.Query()
		var num, offset int
		offsetStr := queryStrings.Get("offset")
		if offsetStr != "" {
			offset, err = strconv.Atoi(offsetStr)
			if err != nil {
				server.BadRequest(w, r, fmt.Errorf("bad offset specified in query string (%q)", offsetStr))
				return
			}
		}
		numStr := queryStrings.Get("n")
		if numStr != "" {
			num, err = strconv.Atoi(numStr)
			if err != nil {
				server.BadRequest(w, r, fmt.Errorf("bad num specified in query string (%q)", numStr))
				return
			}
		}

		labels, err := d.GetLabelsByThreshold(ctx, i, minSize, offset, num)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		w.Header().Set("Content-type", "application/json")
		jsonBytes, err := json.Marshal(labels)
		if err != nil {
			server.BadRequest(w, r, err)
			return
		}
		if _, err := w.Write(jsonBytes); err != nil {
			server.BadRequest(w, r, err)
			return
		}
		timedLog.Infof("HTTP %s: get %d labels for index type %s with threshold %d: %s", r.Method, num, i, t, r.URL)

	default:
		server.BadAPIRequest(w, r, d)
	}
}
