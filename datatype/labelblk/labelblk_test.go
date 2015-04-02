package labelblk

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"log"
	"reflect"
	"sync"
	"testing"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/imageblk"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/tests"

	lz4 "github.com/janelia-flyem/go/golz4"
)

var (
	labelsT, rgbaT datastore.TypeService
	testMu         sync.Mutex
)

// Sets package-level testRepo and TestVersionID
func initTestRepo() (datastore.Repo, dvid.VersionID) {
	testMu.Lock()
	defer testMu.Unlock()
	if labelsT == nil {
		var err error
		labelsT, err = datastore.TypeServiceByName("labelblk")
		if err != nil {
			log.Fatalf("Can't get labelblk type: %s\n", err)
		}
		rgbaT, err = datastore.TypeServiceByName("rgba8blk")
		if err != nil {
			log.Fatalf("Can't get rgba8blk type: %s\n", err)
		}
	}
	return tests.NewRepo()
}

// Data from which to construct repeatable 3d images where adjacent voxels have different values.
var xdata = []uint64{23, 819229, 757, 100303, 9991}
var ydata = []uint64{66599, 201, 881067, 5488, 0}
var zdata = []uint64{1, 734, 43990122, 42, 319596}

// Make a 2d slice of bytes with top left corner at (ox,oy,oz) and size (nx,ny)
func makeSlice(offset dvid.Point3d, size dvid.Point2d) []byte {
	numBytes := size[0] * size[1] * 8
	slice := make([]byte, numBytes, numBytes)
	i := 0
	modz := offset[2] % int32(len(zdata))
	for y := int32(0); y < size[1]; y++ {
		sy := y + offset[1]
		mody := sy % int32(len(ydata))
		sx := offset[0]
		for x := int32(0); x < size[0]; x++ {
			modx := sx % int32(len(xdata))
			binary.BigEndian.PutUint64(slice[i:i+8], xdata[modx]+ydata[mody]+zdata[modz])
			i += 8
			sx++
		}
	}
	return slice
}

// Make a 3d volume of bytes with top left corner at (ox,oy,oz) and size (nx, ny, nz)
func makeVolume(offset, size dvid.Point3d) []byte {
	sliceBytes := size[0] * size[1] * 8
	volumeBytes := sliceBytes * size[2]
	volume := make([]byte, volumeBytes, volumeBytes)
	var i int32
	size2d := dvid.Point2d{size[0], size[1]}
	startZ := offset[2]
	endZ := startZ + size[2]
	for z := startZ; z < endZ; z++ {
		offset[2] = z
		copy(volume[i:i+sliceBytes], makeSlice(offset, size2d))
		i += sliceBytes
	}
	return volume
}

// Creates a new data instance for labelblk
func newDataInstance(repo datastore.Repo, t *testing.T, name dvid.InstanceName) *Data {
	config := dvid.NewConfig()
	dataservice, err := repo.NewData(labelsT, name, config)
	if err != nil {
		t.Errorf("Unable to create labelblk instance %q: %s\n", name, err.Error())
	}
	labels, ok := dataservice.(*Data)
	if !ok {
		t.Errorf("Can't cast labels data service into Data\n")
	}
	return labels
}

func TestLabelblkDirectAPI(t *testing.T) {
	tests.UseStore()
	defer tests.CloseStore()

	repo, versionID := initTestRepo()
	labels := newDataInstance(repo, t, "mylabels")
	labelsCtx := datastore.NewVersionedContext(labels, versionID)

	// Create a fake block-aligned label volume
	offset := dvid.Point3d{32, 0, 64}
	size := dvid.Point3d{96, 64, 160}
	subvol := dvid.NewSubvolume(offset, size)
	data := makeVolume(offset, size)

	// Store it into datastore at root
	v, err := labels.NewVoxels(subvol, data)
	if err != nil {
		t.Fatalf("Unable to make new labels Voxels: %s\n", err.Error())
	}
	if err = labels.PutVoxels(versionID, v, nil); err != nil {
		t.Errorf("Unable to put labels for %s: %s\n", labelsCtx, err.Error())
	}
	if v.NumVoxels() != int64(len(data))/8 {
		t.Errorf("# voxels (%d) after PutVoxels != # original voxels (%d)\n",
			v.NumVoxels(), int64(len(data))/8)
	}

	// Read the stored image
	v2, err := labels.NewVoxels(subvol, nil)
	if err != nil {
		t.Errorf("Unable to make new labels ExtHandler: %s\n", err.Error())
	}
	if err = labels.GetVoxels(versionID, v2, nil); err != nil {
		t.Errorf("Unable to get voxels for %s: %s\n", labelsCtx, err.Error())
	}

	// Make sure the retrieved image matches the original
	if v.Stride() != v2.Stride() {
		t.Errorf("Stride in retrieved subvol incorrect\n")
	}
	if v.Interpolable() != v2.Interpolable() {
		t.Errorf("Interpolable bool in retrieved subvol incorrect\n")
	}
	if !reflect.DeepEqual(v.Size(), v2.Size()) {
		t.Errorf("Size in retrieved subvol incorrect: %s vs expected %s\n",
			v2.Size(), v.Size())
	}
	if v.NumVoxels() != v2.NumVoxels() {
		t.Errorf("# voxels in retrieved is different: %d vs expected %d\n",
			v2.NumVoxels(), v.NumVoxels())
	}
	byteData := v2.Data()

	for i := int64(0); i < v2.NumVoxels()*8; i++ {
		if byteData[i] != data[i] {
			t.Logf("Size of data: %d bytes from GET, %d bytes in PUT\n", len(data), len(data))
			t.Fatalf("GET subvol (%d) != PUT subvol (%d) @ uint64 #%d", byteData[i], data[i], i)
		}
	}
}
func TestLabelblkRepoPersistence(t *testing.T) {
	tests.UseStore()
	defer tests.CloseStore()

	repo, _ := initTestRepo()

	// Make labels and set various properties
	config := dvid.NewConfig()
	config.Set("BlockSize", "12,13,14")
	config.Set("VoxelSize", "1.1,2.8,11")
	config.Set("VoxelUnits", "microns,millimeters,nanometers")
	dataservice, err := repo.NewData(labelsT, "mylabels", config)
	if err != nil {
		t.Errorf("Unable to create labels instance: %s\n", err.Error())
	}
	labels, ok := dataservice.(*Data)
	if !ok {
		t.Errorf("Can't cast labels data service into Data\n")
	}
	oldData := *labels

	// Restart test datastore and see if datasets are still there.
	if err = repo.Save(); err != nil {
		t.Fatalf("Unable to save repo during labels persistence test: %s\n", err.Error())
	}
	oldUUID := repo.RootUUID()
	tests.CloseReopenStore()

	repo2, err := datastore.RepoFromUUID(oldUUID)
	if err != nil {
		t.Fatalf("Can't get repo %s from reloaded test db: %s\n", oldUUID, err.Error())
	}
	dataservice2, err := repo2.GetDataByName("mylabels")
	if err != nil {
		t.Fatalf("Can't get labels instance from reloaded test db: %s\n", err.Error())
	}
	labels2, ok := dataservice2.(*Data)
	if !ok {
		t.Errorf("Returned new data instance 2 is not imageblk.Data\n")
	}
	if !oldData.Equals(labels2) {
		t.Errorf("Expected %v, got %v\n", oldData, *labels2)
	}
}

type labelVol struct {
	size      dvid.Point3d
	blockSize dvid.Point3d
	offset    dvid.Point3d
	name      string
	data      []byte
}

// Create a new label volume and post it to the test datastore.
// Each voxel in volume has sequential labels in X, Y, then Z order.
func (vol *labelVol) postLabelVolume(t *testing.T, uuid dvid.UUID, labelsName, compression string) {
	vol.name = labelsName
	server.CreateTestInstance(t, uuid, "labelblk", labelsName, dvid.Config{})

	nx := vol.size[0] * vol.blockSize[0]
	ny := vol.size[1] * vol.blockSize[1]
	nz := vol.size[2] * vol.blockSize[2]

	vol.data = make([]byte, nx*ny*nz*8)
	var label uint64
	var x, y, z, v int32
	for z = 0; z < nz; z++ {
		for y = 0; y < ny; y++ {
			for x = 0; x < nx; x++ {
				label++
				binary.LittleEndian.PutUint64(vol.data[v:v+8], label)
				v += 8
			}
		}
	}
	apiStr := fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/%d_%d_%d", server.WebAPIPath,
		uuid, labelsName, nx, ny, nz, vol.offset[0], vol.offset[1], vol.offset[2])
	switch compression {
	case "lz4":
		apiStr += "?compression=lz4"
	case "gzip":
		apiStr += "?compression=gzip"
	default:
	}
	server.TestHTTP(t, "POST", apiStr, bytes.NewBuffer(vol.data))
}

func (vol labelVol) testGetLabelVolume(t *testing.T, uuid dvid.UUID, compression string) {

	nx := vol.size[0] * vol.blockSize[0]
	ny := vol.size[1] * vol.blockSize[1]
	nz := vol.size[2] * vol.blockSize[2]

	apiStr := fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/%d_%d_%d", server.WebAPIPath,
		uuid, vol.name, nx, ny, nz, vol.offset[0], vol.offset[1], vol.offset[2])
	switch compression {
	case "lz4":
		apiStr += "?compression=lz4"
	case "gzip":
		apiStr += "?compression=gzip"
	default:
	}
	data := server.TestHTTP(t, "GET", apiStr, nil)
	switch compression {
	case "lz4":
		uncompressed := make([]byte, nx*ny*nz*8)
		if err := lz4.Uncompress(data, uncompressed); err != nil {
			t.Fatalf("Unable to uncompress LZ4 data (%s), %d bytes: %s\n", apiStr, len(data), err.Error())
		}
		data = uncompressed
	case "gzip":
		buf := bytes.NewBuffer(data)
		gr, err := gzip.NewReader(buf)
		if err != nil {
			t.Fatalf("Error on gzip new reader: %s\n", err.Error())
		}
		uncompressed, err := ioutil.ReadAll(gr)
		if err != nil {
			t.Fatalf("Error on reading gzip: %s\n", err.Error())
		}
		if err = gr.Close(); err != nil {
			t.Fatalf("Error on closing gzip: %s\n", err.Error())
		}
		data = uncompressed
	default:
	}
	if len(data) != int(nx*ny*nz*8) {
		t.Errorf("Expected %d uncompressed bytes from 3d labelblk GET.  Got %d instead.", nx*ny*nz*8, len(data))
	}

	// run test to make sure it's same volume as we posted.
	var label uint64
	var x, y, z, v int32
	for z = 0; z < nz; z++ {
		for y = 0; y < ny; y++ {
			for x = 0; x < nx; x++ {
				label++
				got := binary.LittleEndian.Uint64(data[v : v+8])
				if label != got {
					t.Errorf("Error on 3d GET compression (%q): expected %d, got %d\n", compression, label, got)
					return
				}
				v += 8
			}
		}
	}
}

// the label in the test volume should just be the voxel index + 1 when iterating in ZYX order.
// The passed (x,y,z) should be world coordinates, not relative to the volume offset.
func (vol labelVol) label(x, y, z int32) uint64 {
	if x < vol.offset[0] || x >= vol.offset[0]+vol.size[0]*vol.blockSize[0] {
		return 0
	}
	if y < vol.offset[1] || y >= vol.offset[1]+vol.size[1]*vol.blockSize[1] {
		return 0
	}
	if z < vol.offset[2] || z >= vol.offset[2]+vol.size[2]*vol.blockSize[2] {
		return 0
	}
	x -= vol.offset[0]
	y -= vol.offset[1]
	z -= vol.offset[2]
	nx := vol.size[0] * vol.blockSize[0]
	nxy := nx * vol.size[1] * vol.blockSize[1]
	return uint64(z*nxy) + uint64(y*nx) + uint64(x+1)
}

type sliceTester struct {
	orient string
	width  int32
	height int32
	offset dvid.Point3d // offset of slice
}

func (s sliceTester) apiStr(uuid dvid.UUID, name string) string {
	return fmt.Sprintf("%snode/%s/%s/raw/%s/%d_%d/%d_%d_%d", server.WebAPIPath,
		uuid, name, s.orient, s.width, s.height, s.offset[0], s.offset[1], s.offset[2])
}

// make sure the given labels match what would be expected from the test volume.
func (s sliceTester) testLabel(t *testing.T, vol labelVol, img *dvid.Image) {
	data := img.Data()
	var x, y, z int32
	i := 0
	switch s.orient {
	case "xy":
		for y = 0; y < s.height; y++ {
			for x = 0; x < s.width; x++ {
				label := binary.LittleEndian.Uint64(data[i*8 : (i+1)*8])
				i++
				vx := x + s.offset[0]
				vy := y + s.offset[1]
				vz := s.offset[2]
				expected := vol.label(vx, vy, vz)
				if label != expected {
					t.Errorf("Bad label @ (%d,%d,%d): expected %d, got %d\n", vx, vy, vz, expected, label)
					return
				}
			}
		}
		return
	case "xz":
		for z = 0; z < s.height; z++ {
			for x = 0; x < s.width; x++ {
				label := binary.LittleEndian.Uint64(data[i*8 : (i+1)*8])
				i++
				vx := x + s.offset[0]
				vy := s.offset[1]
				vz := z + s.offset[2]
				expected := vol.label(vx, vy, vz)
				if label != expected {
					t.Errorf("Bad label @ (%d,%d,%d): expected %d, got %d\n", vx, vy, vz, expected, label)
					return
				}
			}
		}
		return
	case "yz":
		for z = 0; z < s.height; z++ {
			for y = 0; y < s.width; y++ {
				label := binary.LittleEndian.Uint64(data[i*8 : (i+1)*8])
				i++
				vx := s.offset[0]
				vy := y + s.offset[1]
				vz := z + s.offset[2]
				expected := vol.label(vx, vy, vz)
				if label != expected {
					t.Errorf("Bad label @ (%d,%d,%d): expected %d, got %d\n", vx, vy, vz, expected, label)
					return
				}
			}
		}
		return
	default:
		t.Fatalf("Unknown slice orientation %q\n", s.orient)
	}
}

func TestLabels(t *testing.T) {
	tests.UseStore()
	defer tests.CloseStore()

	uuid := dvid.UUID(server.NewTestRepo(t))
	if len(uuid) < 5 {
		t.Fatalf("Bad root UUID for new repo: %s\n", uuid)
	}

	// Create a labelblk instance
	vol := labelVol{
		size:      dvid.Point3d{5, 5, 5}, // in blocks
		blockSize: dvid.Point3d{32, 32, 32},
		offset:    dvid.Point3d{32, 64, 96},
	}
	vol.postLabelVolume(t, uuid, "labels", "")

	// Verify XY slice.
	sliceOffset := vol.offset
	sliceOffset[0] += 51
	sliceOffset[1] += 11
	sliceOffset[2] += 23
	slice := sliceTester{"xy", 67, 83, sliceOffset}
	apiStr := slice.apiStr(uuid, "labels")
	xy := server.TestHTTP(t, "GET", apiStr, nil)
	img, format, err := dvid.ImageFromBytes(xy, EncodeFormat(), false)
	if err != nil {
		t.Fatalf("Error on XY labels GET: %s\n", err.Error())
	}
	if format != "png" {
		t.Errorf("Expected XY labels GET to return %q image, got %q instead.\n", "png", format)
	}
	if img.NumBytes() != 67*83*8 {
		t.Errorf("Expected %d bytes from XY labelblk GET.  Got %d instead.", 160*160*8, img.NumBytes())
	}
	slice.testLabel(t, vol, img)

	// Verify XZ slice returns what we expect.
	sliceOffset = vol.offset
	sliceOffset[0] += 11
	sliceOffset[1] += 4
	sliceOffset[2] += 3
	slice = sliceTester{"xz", 67, 83, sliceOffset}
	apiStr = slice.apiStr(uuid, "labels")
	xz := server.TestHTTP(t, "GET", apiStr, nil)
	img, format, err = dvid.ImageFromBytes(xz, EncodeFormat(), false)
	if err != nil {
		t.Fatalf("Error on XZ labels GET: %s\n", err.Error())
	}
	if format != "png" {
		t.Errorf("Expected XZ labels GET to return %q image, got %q instead.\n", "png", format)
	}
	if img.NumBytes() != 67*83*8 {
		t.Errorf("Expected %d bytes from XZ labelblk GET.  Got %d instead.", 67*83*8, img.NumBytes())
	}
	slice.testLabel(t, vol, img)

	// Verify YZ slice returns what we expect.
	sliceOffset = vol.offset
	sliceOffset[0] += 7
	sliceOffset[1] += 33
	sliceOffset[2] += 33
	slice = sliceTester{"yz", 67, 83, sliceOffset}
	apiStr = slice.apiStr(uuid, "labels")
	yz := server.TestHTTP(t, "GET", apiStr, nil)
	img, format, err = dvid.ImageFromBytes(yz, EncodeFormat(), false)
	if err != nil {
		t.Fatalf("Error on YZ labels GET: %s\n", err.Error())
	}
	if format != "png" {
		t.Errorf("Expected YZ labels GET to return %q image, got %q instead.\n", "png", format)
	}
	if img.NumBytes() != 67*83*8 {
		t.Errorf("Expected %d bytes from YZ labelblk GET.  Got %d instead.", 67*83*8, img.NumBytes())
	}
	slice.testLabel(t, vol, img)

	// Verify various GET 3d volume with compressions.
	vol.testGetLabelVolume(t, uuid, "")
	vol.testGetLabelVolume(t, uuid, "lz4")
	vol.testGetLabelVolume(t, uuid, "gzip")

	// Create a new ROI instance.
	roiName := "myroi"
	server.CreateTestInstance(t, uuid, "roi", roiName, dvid.Config{})

	// Add ROI data
	apiStr = fmt.Sprintf("%snode/%s/%s/roi", server.WebAPIPath, uuid, roiName)
	server.TestHTTP(t, "POST", apiStr, bytes.NewBufferString(labelsJSON()))

	// Post updated labels without ROI.
	p := make([]byte, 8*blocksz)
	payload := new(bytes.Buffer)
	label = 200000
	for z := 0; z < nz*blocksz; z++ {
		for y := 0; y < ny*blocksz; y++ {
			for x := 0; x < nx; x++ {
				label++
				for i := 0; i < blocksz; i++ {
					binary.LittleEndian.PutUint64(p[i*8:(i+1)*8], label)
				}
				n, err := payload.Write(p)
				if n != 8*blocksz {
					t.Fatalf("Could not write test data: %d bytes instead of %d\n", n, 8*blocksz)
				}
				if err != nil {
					t.Fatalf("Could not write test data: %s\n", err.Error())
				}
			}
		}
	}
	apiStr = fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/32_64_96", server.WebAPIPath,
		uuid, "labels", nx*blocksz, ny*blocksz, nz*blocksz)
	server.TestHTTP(t, "POST", apiStr, payload)

	// Verify 3d volume read returns modified data.
	apiStr = fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/32_64_96", server.WebAPIPath,
		uuid, "labels", nx*blocksz, ny*blocksz, nz*blocksz)
	xyz = server.TestHTTP(t, "GET", apiStr, nil)
	if len(xyz) != 160*160*160*8 {
		t.Errorf("Expected %d bytes from 3d labelblk GET.  Got %d instead.", 160*160*160*8, len(xyz))
	}
	label = 200000
	j = 0
	for z := 0; z < nz*blocksz; z++ {
		for y := 0; y < ny*blocksz; y++ {
			for x := 0; x < nx; x++ {
				label++
				for i := 0; i < blocksz; i++ {
					gotlabel := binary.LittleEndian.Uint64(xyz[j : j+8])
					if gotlabel != label {
						t.Fatalf("Bad label %d instead of expected modified %d at (%d,%d,%d)\n",
							gotlabel, label, x*blocksz+i, y, z)
					}
					j += 8
				}
			}
		}
	}

	// TODO - Use the ROI to retrieve a 2d xy image.

	// TODO - Make sure we aren't getting labels back in non-ROI points.

	// Post again but now with ROI
	payload = new(bytes.Buffer)
	label = 400000
	for z := 0; z < nz*blocksz; z++ {
		for y := 0; y < ny*blocksz; y++ {
			for x := 0; x < nx; x++ {
				label++
				for i := 0; i < blocksz; i++ {
					binary.LittleEndian.PutUint64(p[i*8:(i+1)*8], label)
				}
				n, err := payload.Write(p)
				if n != 8*blocksz {
					t.Fatalf("Could not write test data: %d bytes instead of %d\n", n, 8*blocksz)
				}
				if err != nil {
					t.Fatalf("Could not write test data: %s\n", err.Error())
				}
			}
		}
	}
	apiStr = fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/32_64_96?roi=%s", server.WebAPIPath,
		uuid, "labels", nx*blocksz, ny*blocksz, nz*blocksz, roiName)
	server.TestHTTP(t, "POST", apiStr, payload)

	// Verify ROI masking on GET.
	apiStr = fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/32_64_96?roi=%s", server.WebAPIPath,
		uuid, "labels", nx*blocksz, ny*blocksz, nz*blocksz, roiName)
	xyz2 := server.TestHTTP(t, "GET", apiStr, nil)
	if len(xyz) != 160*160*160*8 {
		t.Errorf("Expected %d bytes from 3d labelblk GET.  Got %d instead.", 160*160*160*8, len(xyz))
	}
	var newlabel uint64 = 400000
	var oldlabel uint64 = 200000
	j = 0
	offsetx := 16
	offsety := 48
	offsetz := 70
	for z := 0; z < nz*blocksz; z++ {
		voxz := z + offsetz
		blockz := voxz / int(imageblk.DefaultBlockSize)
		for y := 0; y < ny*blocksz; y++ {
			voxy := y + offsety
			blocky := voxy / int(imageblk.DefaultBlockSize)
			for x := 0; x < nx; x++ {
				newlabel++
				oldlabel++
				voxx := x*blocksz + offsetx
				for i := 0; i < blocksz; i++ {
					blockx := voxx / int(imageblk.DefaultBlockSize)
					gotlabel := binary.LittleEndian.Uint64(xyz2[j : j+8])
					if inroi(blockx, blocky, blockz) {
						if gotlabel != newlabel {
							t.Fatalf("Got label %d instead of in-ROI label %d at (%d,%d,%d)\n",
								gotlabel, newlabel, voxx, voxy, voxz)
						}
					} else {
						if gotlabel != 0 {
							t.Fatalf("Got label %d instead of 0 at (%d,%d,%d) outside ROI\n",
								gotlabel, voxx, voxy, voxz)
						}
					}
					j += 8
					voxx++
				}
			}
		}
	}

	// Verify everything in mask is new and everything out of mask is old, and everything in mask
	// is new.
	apiStr = fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/32_64_96", server.WebAPIPath,
		uuid, "labels", nx*blocksz, ny*blocksz, nz*blocksz)
	xyz2 = server.TestHTTP(t, "GET", apiStr, nil)
	if len(xyz) != 160*160*160*8 {
		t.Errorf("Expected %d bytes from 3d labelblk GET.  Got %d instead.", 160*160*160*8, len(xyz))
	}
	newlabel = 400000
	oldlabel = 200000
	j = 0
	for z := 0; z < nz*blocksz; z++ {
		voxz := z + offsetz
		blockz := voxz / int(imageblk.DefaultBlockSize)
		for y := 0; y < ny*blocksz; y++ {
			voxy := y + offsety
			blocky := voxy / int(imageblk.DefaultBlockSize)
			for x := 0; x < nx; x++ {
				newlabel++
				oldlabel++
				voxx := x*blocksz + offsetx
				for i := 0; i < blocksz; i++ {
					blockx := voxx / int(imageblk.DefaultBlockSize)
					gotlabel := binary.LittleEndian.Uint64(xyz2[j : j+8])
					if inroi(blockx, blocky, blockz) {
						if gotlabel != newlabel {
							t.Fatalf("Got label %d instead of in-ROI label %d at (%d,%d,%d)\n",
								gotlabel, newlabel, voxx, voxy, voxz)
						}
					} else {
						if gotlabel != oldlabel {
							t.Fatalf("Got label %d instead of label %d at (%d,%d,%d) outside ROI\n",
								gotlabel, oldlabel, voxx, voxy, voxz)
						}
					}
					j += 8
					voxx++
				}
			}
		}
	}
}
