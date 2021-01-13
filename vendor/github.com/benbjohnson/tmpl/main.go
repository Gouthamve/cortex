package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig"
	"github.com/dustin/go-humanize/english"
)

// Extension is the required file extension for processed files.
const Extension = ".tmpl"

func main() {
	m := NewMain()
	if err := m.ParseFlags(os.Args[1:]); err != nil {
		fmt.Fprintln(m.Stderr, err)
		os.Exit(2)
	}

	if err := m.Run(); err != nil {
		fmt.Fprintln(m.Stderr, err)
		os.Exit(1)
	}
}

type Main struct {
	// Files to be processed.
	Paths []string

	NoHeader   bool
	OutputPath string

	// Data to be applied to the files during generation.
	Data interface{}

	OS interface {
		Stat(filename string) (os.FileInfo, error)
	}

	FileReadWriter interface {
		ReadFile(filename string) ([]byte, error)
		WriteFile(filename string, data []byte, perm os.FileMode) error
	}

	// Standard input/output
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// NewMain returns a new instance of Main.
func NewMain() *Main {
	return &Main{
		OS:             &mainOS{},
		FileReadWriter: &fileReadWriter{},

		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// ParseFlags parses the command line flags from args.
func (m *Main) ParseFlags(args []string) error {
	fs := flag.NewFlagSet("tmp", flag.ContinueOnError)
	fs.SetOutput(m.Stderr)
	data := fs.String("data", "", "json data")
	fs.BoolVar(&m.NoHeader, "no-header", false, "hide warning header")
	fs.StringVar(&m.OutputPath, "o", "", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Parse JSON data.
	if *data != "" {
		// If the data has a @-prefix then read from a file.
		buf := []byte(*data)
		if strings.HasPrefix(*data, "@") {
			b, err := m.FileReadWriter.ReadFile(strings.TrimPrefix(*data, "@"))
			if err != nil {
				return err
			}
			buf = b
		}

		if err := json.Unmarshal(buf, &m.Data); err != nil {
			return err
		}
	}

	// All arguments are considered paths to process.
	m.Paths = fs.Args()

	return nil
}

// Run executes the program.
func (m *Main) Run() error {
	// Verify we have at least one path.
	if len(m.Paths) == 0 {
		return errors.New("path required")
	}

	// Process each path.
	for _, path := range m.Paths {
		if err := m.process(path); err != nil {
			return err
		}
	}

	return nil
}

// process reads a template file from path, processes it, and writes it to its generated path.
func (m *Main) process(path string) error {
	// Validate that we have a prefix we can strip off for the generated path.
	if !strings.HasSuffix(path, Extension) {
		return fmt.Errorf("path must have %s extension: %s", Extension, path)
	}
	outputPath := m.OutputPath
	if outputPath == "" {
		outputPath = strings.TrimSuffix(path, Extension)
	}

	// Stat the file to retrieve the mode.
	fi, err := m.OS.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("file not found")
	} else if err != nil {
		return err
	}

	// Read in template file.
	source, err := m.FileReadWriter.ReadFile(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("file not found")
	} else if err != nil {
		return err
	}

	// Build function map.
	funcMap := sprig.TxtFuncMap()
	funcMap["pluralize"] = pluralize

	// Parse file into template.
	tmpl, err := template.New("main").Funcs(funcMap).Parse(string(source))
	if err != nil {
		return err
	}

	// Create a comment at the top if generating to a .go file.
	var buf bytes.Buffer
	if !m.NoHeader {
		switch filepath.Ext(outputPath) {
		case ".go":
			fmt.Fprintln(&buf, "// Generated by tmpl")
			fmt.Fprintln(&buf, "// https://github.com/benbjohnson/tmpl")
			fmt.Fprintln(&buf, "//")
			fmt.Fprintln(&buf, "// DO NOT EDIT!")
			fmt.Fprintln(&buf, "// Source:", path)
			fmt.Fprintln(&buf, "")
		}
	}

	// Execute template.
	if err := tmpl.Execute(&buf, m.Data); err != nil {
		return err
	}

	// Format output if it's a Go file.
	// If there is an error during formatting then simply output unformatted Go.
	output := buf.Bytes()
	switch filepath.Ext(outputPath) {
	case ".go":
		formatted, err := format.Source(output)
		if err != nil {
			m.FileReadWriter.WriteFile(outputPath, output, fi.Mode())
			return err
		}
		output = formatted
	}

	// Write buffer to file.
	if err := m.FileReadWriter.WriteFile(outputPath, output, fi.Mode()); err != nil {
		return err
	}

	return nil
}

func pluralize(s string) string {
	return english.PluralWord(2, s, "")
}

// fileReadWriter implements Main.FileReadWriter.
type fileReadWriter struct{}

func (*fileReadWriter) ReadFile(filename string) ([]byte, error) {
	return ioutil.ReadFile(filename)
}
func (*fileReadWriter) WriteFile(filename string, data []byte, perm os.FileMode) error {
	return ioutil.WriteFile(filename, data, perm)
}

// mainOS implements Main.OS.
type mainOS struct{}

func (*mainOS) Stat(name string) (os.FileInfo, error) { return os.Stat(name) }
