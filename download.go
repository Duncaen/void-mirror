package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/exp/slog"
)

type DownloadResult struct {
	DownloadFile *Download
	err          error
}

type Download struct {
	URL       *url.URL
	Filename  string
	Directory string
	SHA256    []byte
}

func (dl *Download) Download(ctx context.Context, req *http.Request) (err error) {
	req = req.Clone(ctx)
	req.URL = dl.URL
	slog.Debug("downloading", slog.Group("req", "url", req.URL))
	var resp *http.Response
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	defer func() {
		attr := []any{
				slog.Group("req", "url", req.URL),
				slog.Group("resp", "status", resp.StatusCode),
		}
		if err != nil {
			attr = append(attr, slog.Any("error", err))
			slog.Info("download failed", attr...)
		} else {
			slog.Info("download finished", attr...)
		}
	}()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	default:
		slog.Error("unexpected status code", slog.Group("req", "url", req.URL), slog.Group("resp", "status", resp.Status))
		return nil
	case http.StatusOK:
		// fallthrough
	}

	var rd io.Reader
	rd = resp.Body
	var hash hash.Hash
	if len(dl.SHA256) != 0 {
		hash = sha256.New()
		rd = io.TeeReader(rd, hash)
	}

	pattern := fmt.Sprintf(".%s.*", dl.Filename)
	tmpfile, err := Tempfile(dl.Directory, pattern)
	if err != nil {
		slog.Error("could not create tmp file", "directory", dl.Directory, "error", err)
		return err
	}
	defer tmpfile.Remove()
	tmpfilename := tmpfile.file.Name()

	_, err = io.Copy(tmpfile.file, rd)
	if err != nil {
		slog.Error("writing tmpfile failed", "path", tmpfilename, "error", err)
		return err
	}
	if err = resp.Body.Close(); err != nil {
		slog.Error("closing response body failed", "error", err)
		err = nil
	}

	if hash != nil {
		sum := hash.Sum(nil)
		if !bytes.Equal(sum[:], dl.SHA256) {
			slog.Error("hash mismatch", "expected", hex.EncodeToString(dl.SHA256), "got", hex.EncodeToString(sum))
			return fmt.Errorf("hash mismatch")
		}
	}

	// set the modtime on the temporary file
	if value := resp.Header.Get("Last-Modified"); value != "" {
		var date time.Time
		if date, err = time.Parse(http.TimeFormat, value); err == nil {
			if err := os.Chtimes(tmpfilename, time.Now(), date); err != nil {
				slog.Error("changing mod time failed", "path", tmpfilename, "error", err)
				err = nil
			}
		}
	}

	// everything fine, rename the temporary file
	destfile := filepath.Join(dl.Directory, dl.Filename)
	if err = tmpfile.Commit(filepath.Join(dl.Directory, dl.Filename)); err != nil {
		slog.Error("renaming tempfile", "oldpath", tmpfilename, "newpath", destfile, "error", err)
		return err
	}
	return nil
}

