package promql

import (
	"path/filepath"
	"testing"
)

func TestStuff(t *testing.T) {
	files, err := filepath.Glob("testdata/*.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range files {
		test, err := NewTestFromFile(t, fn)
		if err != nil {
			t.Errorf("error creating test for %s: %s", fn, err)
		}
		err = test.Run()
		if err != nil {
			t.Error("error running test %s: %s", fn, err)
		}
		test.Close()
	}
}
