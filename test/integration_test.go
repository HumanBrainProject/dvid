/*
	The test package tests a variety of data types in an integrated fashion.
*/
package test

import (
	. "github.com/janelia-flyem/go/gocheck"
	"testing"

	"github.com/janelia-flyem/dvid/datastore"
	_ "github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"

	// Declare the data types this DVID executable will support
	_ "github.com/janelia-flyem/dvid/datatype/grayscale8"
	_ "github.com/janelia-flyem/dvid/datatype/labels32"
	_ "github.com/janelia-flyem/dvid/datatype/labels64"
	_ "github.com/janelia-flyem/dvid/datatype/rgba8"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { TestingT(t) }

type DataSuite struct {
	dir     string
	service *server.Service
	head    datastore.UUID
}

var _ = Suite(&DataSuite{})

// This will setup a new datastore and open it up, keeping the UUID and
// service pointer in the DataSuite.
func (suite *DataSuite) SetUpSuite(c *C) {
	// Make a temporary testing directory that will be auto-deleted after testing.
	suite.dir = c.MkDir()

	// Create a new datastore.
	err := datastore.Init(suite.dir, true)
	c.Assert(err, IsNil)

	// Open the datastore
	suite.service, err = server.OpenDatastore(suite.dir)
	c.Assert(err, IsNil)
}

func (suite *DataSuite) TearDownSuite(c *C) {
	suite.service.Shutdown()
}

func (suite *DataSuite) TestVersionedDataOps(c *C) {
	root1, _, err := suite.service.NewDataset()
	c.Assert(err, IsNil)

	versioned := true
	err = suite.service.NewData(root1, "grayscale8", "grayscale", versioned)
	c.Assert(err, IsNil)

	err = suite.service.NewData(root1, "labels64", "labels", versioned)
	c.Assert(err, IsNil)

	child1, err := suite.service.NewVersion(root1)
	// Should be an error because we have not locked previous node before making a child.
	c.Assert(err, NotNil)

	err = suite.service.Lock(root1)
	c.Assert(err, IsNil)

	child1, err = suite.service.NewVersion(root1)
	c.Assert(err, IsNil)

	// Add a second Dataset
	root2, _, err := suite.service.NewDataset()
	c.Assert(err, IsNil)

	c.Assert(root1, Not(Equals), root2)

	err = suite.service.NewData(root2, "labels64", "labels2", versioned)
	c.Assert(err, IsNil)

	err = suite.service.NewData(root2, "grayscale8", "grayscale2", versioned)
	c.Assert(err, IsNil)

	err = suite.service.Lock(root2)
	c.Assert(err, IsNil)

	child2, err := suite.service.NewVersion(root2)
	c.Assert(err, IsNil)

	c.Assert(child1, Not(Equals), child2)
}

// Make sure Datasets configuration persists even after shutdown.
func (suite *DataSuite) TestDatasetPersistence(c *C) {
	dir := c.MkDir()

	// Create a new datastore.
	err := datastore.Init(dir, true)
	c.Assert(err, IsNil)

	// Open the datastore
	service, err := datastore.Open(dir)
	c.Assert(err, IsNil)

	root, _, err := service.NewDataset()
	c.Assert(err, IsNil)

	err = service.NewData(root, "grayscale8", "node1image", false)
	c.Assert(err, IsNil)

	root, _, err = service.NewDataset()
	c.Assert(err, IsNil)

	err = service.NewData(root, "grayscale8", "node2image", false)
	c.Assert(err, IsNil)

	oldJSON, err := service.DatasetsJSON()
	c.Assert(err, IsNil)

	service.Shutdown()

	// Open using different service
	service2, err := datastore.Open(dir)
	c.Assert(err, IsNil)

	newJSON, err := service2.DatasetsJSON()
	c.Assert(err, IsNil)

	c.Assert(newJSON, DeepEquals, oldJSON)
}