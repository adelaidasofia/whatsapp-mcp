package main

import (
	"io"
	"testing"
	"time"
)

func TestWatchParentClosesWhenTheAppsPipeCloses(t *testing.T) {
	r, w := io.Pipe()
	gone := watchParent(true, r)
	select {
	case <-gone:
		t.Fatal("reported the app gone while its pipe was still open")
	case <-time.After(50 * time.Millisecond):
	}
	_ = w.Close() // what the OS does when the app exits or crashes
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("did not notice the app's pipe closing")
	}
}

func TestWatchParentDisabledNeverFires(t *testing.T) {
	r, w := io.Pipe()
	_ = w.Close()
	select {
	case <-watchParent(false, r):
		t.Fatal("a bridge started by hand must not exit when its stdin closes")
	case <-time.After(50 * time.Millisecond):
	}
}
