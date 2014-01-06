/*
	Package labels64 tailors the voxels data type for 64-bit labels and allows loading
	of NRGBA images (e.g., Raveler superpixel PNG images) that implicitly use slice Z as
	part of the label index.
*/
package labels64

import (
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"strings"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/voxels"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
)

const (
	Version = "0.1"
	RepoUrl = "github.com/janelia-flyem/dvid/datatype/labels64"
)

const HelpMessage = `
API for datatypes derived from labels64 (github.com/janelia-flyem/dvid/datatype/labels64)
=========================================================================

Command-line:

$ dvid dataset <UUID> new labels64 <data name> <settings...>

	Adds newly named data of the 'type name' to dataset with specified UUID.

	Example:

	$ dvid dataset 3f8c new labels64 superpixels Res=1.5,1.0,1.5

    Arguments:

    UUID           Hexidecimal string with enough characters to uniquely identify a version node.
    data name      Name of data to create, e.g., "superpixels"
    settings       Configuration settings in "key=value" format separated by spaces.

    Configuration Settings (case-insensitive keys)

    Versioned      "true" or "false" (default)
    BlockSize      Size in pixels  (default: %s)
    Res       Resolution of voxels (default: 1.0, 1.0, 1.0)
    Units  String of units (default: "nanometers")

$ dvid node <UUID> <data name> load raveler <offset> <image glob>

    Initializes version node to a set of XY label images described by glob of filenames.
    The DVID server must have access to the named files.  Currently, XY images are required.
    Requires the files are either 32-bit RGBA (fills the lower 4 bytes of the 64-bit label
    while the image Z fills the higher 4 bytes) or 64-bit RGBA.

    Example: 

    $ dvid node 3f8c superpixels load 0,0,100 "data/*.png"

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    offset        3d coordinate in the format "x,y,z".  Gives coordinate of top upper left voxel.
    image glob    Filenames of label images, preferably in quotes, e.g., "foo-xy-*.png"
	
    ------------------

HTTP API (Level 2 REST):

GET  /api/node/<UUID>/<data name>/help

	Returns data-specific help message.


GET  /api/node/<UUID>/<data name>/info
POST /api/node/<UUID>/<data name>/info

    Retrieves or puts DVID-specific data properties for these voxels.

    Example: 

    GET /api/node/3f8c/grayscale/info

    Returns JSON with configuration settings that include location in DVID space and
    min/max block indices.

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of voxels data.


GET  /api/node/<UUID>/<data name>/schema

	Retrieves a JSON schema (application/vnd.dvid-nd-data+json) that describes the layout
	of bytes returned for n-d images.


GET  /api/node/<UUID>/<data name>/<dims>/<size>/<offset>[/<format>]
POST /api/node/<UUID>/<data name>/<dims>/<size>/<offset>[/<format>]

    Retrieves or puts label data as binary blob using schema above.  Binary data is simply
    packed 64-bit data.

    Example: 

    GET /api/node/3f8c/superpixels/0_1/512_256/0_0_100

    Returns an XY slice (0th and 1st dimensions) with width (x) of 512 voxels and
    height (y) of 256 voxels with offset (0,0,100) in binary format.
    The example offset assumes the "grayscale" data in version node "3f8c" is 3d.
    The "Content-type" of the HTTP response will be "application/octet-stream".

    Arguments:

    UUID          Hexidecimal string with enough characters to uniquely identify a version node.
    data name     Name of data to add.
    dims          The axes of data extraction in form "i_j_k,..."  Example: "0_2" can be XZ.
                    Slice strings ("xy", "xz", or "yz") are also accepted.
    size          Size in voxels along each dimension specified in <dims>.
    offset        Gives coordinate of first voxel using dimensionality of data.
`

func init() {
	values := voxels.DataValues{
		{
			DataType: "uint64",
			Label:    "labels64",
		},
	}
	dtype := &Datatype{voxels.NewDatatype(values)}
	dtype.DatatypeID = datastore.MakeDatatypeID("labels64", RepoUrl, Version)
	datastore.RegisterDatatype(dtype)

	// See doc for package on why channels are segregated instead of interleaved.
	// Data types must be registered with the datastore to be used.
	datastore.RegisterDatatype(dtype)

	// Need to register types that will be used to fulfill interfaces.
	gob.Register(&Datatype{})
	gob.Register(&Data{})
}

// -------  ExtHandler interface implementation -------------

// Labels is an image volume that fulfills the voxels.ExtHandler interface.
type Labels struct {
	*voxels.Voxels
}

func (l *Labels) String() string {
	return fmt.Sprintf("Labels of size %s @ offset %s", l.Size(), l.StartPoint())
}

// --- Labels64 Datatype -----

// Datatype just uses voxels data type by composition.
type Datatype struct {
	*voxels.Datatype
}

// --- TypeService interface ---

// NewData returns a pointer to a new labels64 with default values.
func (dtype *Datatype) NewDataService(id *datastore.DataID, config dvid.Config) (
	datastore.DataService, error) {

	voxelservice, err := dtype.Datatype.NewDataService(id, config)
	if err != nil {
		return nil, err
	}
	service := &Data{
		Data: *(voxelservice.(*voxels.Data)),
	}
	return service, nil
}

func (dtype *Datatype) Help() string {
	return HelpMessage
}

// Data of labels64 type just uses voxels.Data.
type Data struct {
	voxels.Data
}

// JSONString returns the JSON for this Data's configuration
func (d *Data) JSONString() (string, error) {
	m, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(m), nil
}

// --- voxels.IntHandler interface -------------

// NewExtHandler returns a labels64 ExtHandler given some geometry and optional image data.
// If img is passed in, the function will initialize the ExtHandler with data from the image.
// Otherwise, it will allocate a zero buffer of appropriate size.
// Unlike the standard voxels NewExtHandler, the labels64 version will modify the
// labels based on the z-coordinate of the given geometry.
func (d *Data) NewExtHandler(geom dvid.Geometry, img interface{}) (voxels.ExtHandler, error) {
	bytesPerVoxel := d.Properties.Values.BytesPerVoxel()
	stride := geom.Size().Value(0) * bytesPerVoxel
	var data []byte

	if img == nil {
		data = make([]byte, int64(bytesPerVoxel)*geom.NumVoxels())
	} else {
		switch t := img.(type) {
		case image.Image:
			var voxelSize, actualStride int32
			var err error
			data, voxelSize, actualStride, err = dvid.ImageData(t)
			if err != nil {
				return nil, err
			}
			if voxelSize != 4 && voxelSize != 8 {
				return nil, fmt.Errorf("Expecting 4 or 8 byte/voxel labels, got %d bytes/voxel!", voxelSize)
			}
			if voxelSize == 4 {
				data, err = d.addLabelZ(geom, data, actualStride)
				if err != nil {
					return nil, err
				}
			} else if actualStride < stride {
				return nil, fmt.Errorf("Too little data in input image (expected stride %d)", stride)
			} else {
				stride = actualStride
			}
		default:
			return nil, fmt.Errorf("Unexpected image type given to NewExtHandler(): %T", t)
		}
	}

	labels := &Labels{
		voxels.NewVoxels(geom, d.Properties.Values, data, stride, d.ByteOrder),
	}
	return labels, nil
}

// Convert a 32-bit label into a 64-bit label by adding the Z coordinate into high 32 bits.
// Also drops the high byte (alpha channel) since Raveler labels only use 24-bits.
func (d *Data) addLabelZ(geom dvid.Geometry, data32 []uint8, stride int32) ([]byte, error) {
	if len(data32)%4 != 0 {
		return nil, fmt.Errorf("Expected 4 byte/voxel alignment but have %d bytes!", len(data32))
	}
	coord := geom.StartPoint()
	if coord.NumDims() < 3 {
		return nil, fmt.Errorf("Expected n-d (n >= 3) offset for image.  Got %d dimensions.",
			coord.NumDims())
	}
	zeroSuperpixelBytes := make([]byte, 8, 8)
	superpixelBytes := make([]byte, 8, 8)
	binary.BigEndian.PutUint32(superpixelBytes[0:4], uint32(coord.Value(2)))

	nx := int(geom.Size().Value(0))
	ny := int(geom.Size().Value(1))
	numBytes := nx * ny * 8
	data64 := make([]byte, numBytes, numBytes)
	dstI := 0
	for y := 0; y < ny; y++ {
		srcI := y * int(stride)
		for x := 0; x < nx; x++ {
			if data32[srcI] == 0 && data32[srcI+1] == 0 && data32[srcI+2] == 0 {
				copy(data64[dstI:dstI+8], zeroSuperpixelBytes)
			} else {
				superpixelBytes[5] = data32[srcI+2]
				superpixelBytes[6] = data32[srcI+1]
				superpixelBytes[7] = data32[srcI]
				copy(data64[dstI:dstI+8], superpixelBytes)
			}
			// NOTE: we skip the 4th byte (alpha) at srcI+3
			//a := uint32(data32[srcI+3])
			//b := uint32(data32[srcI+2])
			//g := uint32(data32[srcI+1])
			//r := uint32(data32[srcI+0])
			//spid := (b << 16) | (g << 8) | r
			srcI += 4
			dstI += 8
		}
	}
	return data64, nil
}

func RavelerSuperpixelBytes(slice, superpixel32 uint32) []byte {
	b := make([]byte, 8, 8)
	if superpixel32 != 0 {
		binary.BigEndian.PutUint32(b[0:4], slice)
		binary.BigEndian.PutUint32(b[4:8], superpixel32)
	}
	return b
}

// --- datastore.DataService interface ---------

// DoRPC acts as a switchboard for RPC commands.
func (d *Data) DoRPC(request datastore.Request, reply *datastore.Response) error {
	switch request.TypeCommand() {
	case "load":
		if len(request.Command) < 5 {
			return fmt.Errorf("Poorly formatted load command.  See command-line help.")
		}
		// Parse the request
		var uuidStr, dataName, cmdStr, formatStr, offsetStr string
		filenames, err := request.FilenameArgs(1, &uuidStr, &dataName, &cmdStr, &formatStr, &offsetStr)
		if err != nil {
			return err
		}
		if len(filenames) == 0 {
			return fmt.Errorf("Need to include at least one file to add: %s", request)
		}

		// Get offset
		offset, err := dvid.StringToPoint(offsetStr, ",")
		if err != nil {
			return fmt.Errorf("Illegal offset specification: %s: %s", offsetStr, err.Error())
		}

		// Get list of files to add
		var addedFiles string
		if len(filenames) == 1 {
			addedFiles = filenames[0]
		} else {
			addedFiles = fmt.Sprintf("filenames: %s [%d more]", filenames[0], len(filenames)-1)
		}
		dvid.Log(dvid.Debug, addedFiles+"\n")

		// Get version node
		uuid, err := server.MatchingUUID(uuidStr)
		if err != nil {
			return err
		}
		if formatStr == "raveler" {
			return voxels.LoadXY(d, uuid, offset, filenames)
		} else {
			return fmt.Errorf("Currently, only Raveler loading is supported for 64-bit labels.")
		}

	default:
		return d.UnknownCommand(request)
	}
	return nil
}

// DoHTTP handles all incoming HTTP requests for this data.
func (d *Data) DoHTTP(uuid dvid.UUID, w http.ResponseWriter, r *http.Request) error {
	startTime := time.Now()

	// Allow cross-origin resource sharing.
	w.Header().Add("Access-Control-Allow-Origin", "*")

	// Get the action (GET, POST)
	action := strings.ToLower(r.Method)
	var op voxels.OpType
	switch action {
	case "get":
		op = voxels.GetOp
	case "post":
		op = voxels.PutOp
	default:
		return fmt.Errorf("Can only handle GET or POST HTTP verbs")
	}

	// Break URL request into arguments
	url := r.URL.Path[len(server.WebAPIPath):]
	parts := strings.Split(url, "/")

	// Process help and info.
	switch parts[3] {
	case "help":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, d.Help())
		return nil
	case "schema":
		jsonStr, err := d.NdDataSchema()
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return err
		}
		w.Header().Set("Content-Type", "application/vnd.dvid-nd-data+json")
		fmt.Fprintln(w, jsonStr)
		return nil
	case "info":
		jsonStr, err := d.JSONString()
		if err != nil {
			server.BadRequest(w, r, err.Error())
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, jsonStr)
		return nil
	default:
	}

	// Get the data shape.
	shapeStr := dvid.DataShapeString(parts[3])
	dataShape, err := shapeStr.DataShape()
	if err != nil {
		return fmt.Errorf("Bad data shape given '%s'", shapeStr)
	}

	switch dataShape.ShapeDimensions() {
	case 2:
		sizeStr, offsetStr := parts[4], parts[5]
		slice, err := dvid.NewSliceFromStrings(shapeStr, offsetStr, sizeStr, "_")
		if err != nil {
			return err
		}
		if op == voxels.PutOp {
			// TODO -- Put in format checks for POSTed image.
			postedImg, _, err := dvid.ImageFromPost(r, "image")
			if err != nil {
				return err
			}
			e, err := d.NewExtHandler(slice, postedImg)
			if err != nil {
				return err
			}
			err = voxels.PutImage(uuid, d, e)
			if err != nil {
				return err
			}
		} else {
			e, err := d.NewExtHandler(slice, nil)
			if err != nil {
				return err
			}
			img, err := voxels.GetImage(uuid, d, e)
			if err != nil {
				return err
			}
			var formatStr string
			if len(parts) >= 7 {
				formatStr = parts[6]
			}
			//dvid.ElapsedTime(dvid.Normal, startTime, "%s %s upto image formatting", op, slice)
			err = dvid.WriteImageHttp(w, img, formatStr)
			if err != nil {
				return err
			}
		}
	case 3:
		sizeStr, offsetStr := parts[4], parts[5]
		subvol, err := dvid.NewSubvolumeFromStrings(offsetStr, sizeStr, "_")
		if err != nil {
			return err
		}
		if op == voxels.GetOp {
			e, err := d.NewExtHandler(subvol, nil)
			if err != nil {
				return err
			}
			if data, err := voxels.GetVolume(uuid, d, e); err != nil {
				return err
			} else {
				w.Header().Set("Content-type", "application/octet-stream")
				_, err = w.Write(data)
				if err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("DVID does not yet support POST of volume data")
		}
	default:
		return fmt.Errorf("DVID currently supports shapes of only 2 and 3 dimensions")
	}

	dvid.ElapsedTime(dvid.Debug, startTime, "HTTP %s: %s (%s)", r.Method, dataShape, r.URL)
	return nil
}