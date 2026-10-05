package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	off := ""
	testPortalURL = &off
	os.Exit(m.Run())
}
