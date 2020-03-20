package sconf

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"reflect"
)

// ParseFile reads an sconf file from path into dst.
func ParseFile(path string, dst interface{}) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	return parse(path, src, dst)
}

// Parse reads an sconf file from a reader into dst.
func Parse(src io.Reader, dst interface{}) error {
	return parse("", src, dst)
}

// Describe writes an example sconf file describing v to w. The file includes all
// fields and documentation on the fields as configured with the "sconf-doc" tags.
func Describe(w io.Writer, v interface{}) error {
	return describe(w, v, true)
}

// Write writes a valid sconf file describing v to w, without comments and without
// optional fields set to their zero values.
func Write(w io.Writer, v interface{}) error {
	return describe(w, v, false)
}

func describe(w io.Writer, v interface{}, full bool) (err error) {
	value := reflect.ValueOf(v)
	t := value.Type()
	if t.Kind() == reflect.Ptr {
		value = value.Elem()
		t = value.Type()
	}
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("top level object must be a struct, is a %T", v)
	}
	defer func() {
		x := recover()
		if x == nil {
			return
		}
		if e, ok := x.(writeError); ok {
			err = error(e)
		} else {
			panic(x)
		}
	}()
	wr := &writer{out: bufio.NewWriter(w), full: full}
	wr.describeStruct(value)
	wr.flush()
	return nil
}
