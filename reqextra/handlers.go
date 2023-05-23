package reqextra

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/carlmjohnson/requests"
)

func ToTemp(dir, pattern string, tmpfile *string) requests.ResponseHandler {
	return func(resp *http.Response) error {
		file, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return err
		}
		*tmpfile = file.Name()
		if _, err := io.Copy(file, resp.Body); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return nil
	}
}

func ToFileAtomic(path string) requests.ResponseHandler {
	return func(resp *http.Response) error {
		dir := filepath.Dir(path)
		pattern := fmt.Sprintf(".%s.*", filepath.Base(path))
		file, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return err
		}
		tmpfile := file.Name()
		if _, err := io.Copy(file, resp.Body); err != nil {
			file.Close()
			os.Remove(tmpfile)
			return err
		}
		if err := file.Close(); err != nil {
			os.Remove(tmpfile)
			return err
		}
		return os.Rename(tmpfile, path)
	}
}

type closer struct {
	rd io.Reader
	close func() error
}

func (c *closer) Read(p []byte) (int, error) {
	return c.rd.Read(p)
}

func (c *closer) Close() error {
	return c.close()
}

func HashResponse(hash hash.Hash, handler requests.ResponseHandler) requests.ResponseHandler {
	return func(resp *http.Response) error {
		resp.Body = &closer{io.TeeReader(resp.Body, hash), resp.Body.Close}
		return handler(resp)
	}
}

func Sha256Verify(sum []byte, handler requests.ResponseHandler) requests.ResponseHandler {
	hash := sha256.New()
	handler = HashResponse(hash, handler)
	return func(resp *http.Response) error {
		if err := handler(resp); err != nil {
			return err
		}
		res := hash.Sum(nil)
		if !bytes.Equal(res, sum) {
			return fmt.Errorf("%w: checksum mismatch: got %q, expected %q",
				(*requests.ResponseError)(resp), hex.EncodeToString(res), hex.EncodeToString(sum))
		}
		return nil
	}
}

func CopyCacheHeaders(etag, lastModified *string) requests.ResponseHandler {
	return func (resp *http.Response) error {
		*etag = resp.Header.Get("ETag")
		*lastModified = resp.Header.Get("Last-Modified")
		return nil
	}
}
