package main

import "os"

type tmpfile struct {
	file *os.File
}

// Tempfile create a new templ
func Tempfile(dir, pattern string) (*tmpfile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &tmpfile{file}, nil
}

func (t *tmpfile) Remove() error {
	if t.file == nil {
		return nil
	}
	t.file.Close()
	return os.Remove(t.file.Name())
}

func (t *tmpfile) Commit(path string) error {
	if err := t.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(t.file.Name(), path); err != nil {
		return err
	}
	t.file = nil
	return nil
}
