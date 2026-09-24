package modal

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// responseCapture turns one translated Modal iteration back into an
// *http.Response for the web-search orchestrator. It is used only on search
// turns; ordinary Modal turns retain their direct streaming path.
type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: make(http.Header)}
}

func (c *responseCapture) Header() http.Header { return c.header }

func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *responseCapture) Write(data []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len()+len(data) > maxTranslatedPayload {
		c.err = errors.New("translated Modal response exceeds adapter limit")
		return 0, c.err
	}
	return c.body.Write(data)
}

func (c *responseCapture) Flush() {}

func (c *responseCapture) response() (*http.Response, error) {
	if c.err != nil {
		return nil, c.err
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     c.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(c.body.Bytes())),
	}, nil
}
