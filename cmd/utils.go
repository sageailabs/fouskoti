// Copyright © The Sage Group plc or its licensors.

package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

func appendDocSeparator(inputs []io.Reader) []io.Reader {
	if len(inputs) > 0 {
		inputs = append(inputs, bytes.NewBufferString("\n---\n"))
	}
	return inputs
}

type yamlInputReader struct {
	closers []io.Closer
	reader  io.Reader
}

func (reader *yamlInputReader) Close() error {
	var err error
	for _, closer := range reader.closers {
		currentErr := closer.Close()
		if err == nil && currentErr != nil {
			err = currentErr
		}
	}
	return err
}

func (reader *yamlInputReader) Read(p []byte) (int, error) {
	return reader.reader.Read(p)
}

// Opens all input files and combines them in a single YAML
// stream for reading.  Uses stdin if no args are provided.
func getYAMLInputReader(args []string) (io.ReadCloser, error) {
	var closers []io.Closer
	var inputs []io.Reader
	for _, arg := range args {
		if arg == "-" {
			inputs = append(inputs, os.Stdin)
		} else {
			inputs = appendDocSeparator(inputs)
			file, err := os.Open(arg)
			if err != nil {
				(&yamlInputReader{closers: closers}).Close()
				return nil, fmt.Errorf("unable to open input file %s: %w", arg, err)
			}
			closers = append(closers, file)
			inputs = appendDocSeparator(inputs)
			inputs = append(inputs, file)
		}
	}
	if len(args) == 0 {
		inputs = append(inputs, os.Stdin)
	}
	return &yamlInputReader{
		closers: closers,
		reader:  io.MultiReader(inputs...),
	}, nil
}
