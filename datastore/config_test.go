package datastore

import (
	. "github.com/DocSavage/gocheck"
	_ "testing"
)

func (suite *DataSuite) TestGet(c *C) {
	var config runtimeConfig
	err := config.Get(suite.service.db)
	c.Assert(err, IsNil)
}