package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// Multipart upload exists because the platform streams objects whose size
// is not known in advance: a snapshot's size depends on how much the
// sandbox wrote. A single PUT would require buffering the whole thing to
// set Content-Length, which for a multi-gigabyte checkpoint is not
// acceptable — so parts are uploaded as the stream produces them.

// DefaultPartSize is the buffer each part fills before being uploaded.
// S3 requires at least 5 MiB for all but the final part; 16 MiB keeps the
// request count reasonable for large objects without holding much memory.
const DefaultPartSize = 16 << 20

// Uploader writes an object as a multipart upload. It satisfies
// io.WriteCloser, so callers stream into it and Close publishes the object.
// Nothing is visible at the destination key until Close succeeds, which is
// the property that keeps a failed snapshot from leaving a readable
// partial blob.
type Uploader struct {
	ctx      context.Context
	client   *Client
	bucket   string
	key      string
	uploadID string
	partSize int

	buf   []byte
	parts []completedPart

	mu     sync.Mutex
	closed bool
	failed error
}

type completedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	UploadID string   `xml:"UploadId"`
}

type completeRequest struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

// NewUploader starts a multipart upload.
func (c *Client) NewUploader(ctx context.Context, bucket, key string) (*Uploader, error) {
	u, err := url.Parse(c.objectURL(bucket, key))
	if err != nil {
		return nil, err
	}
	u.RawQuery = "uploads="

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(req, nil)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	drain(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("s3: initiate upload %s/%s: HTTP %d: %s",
			bucket, key, resp.StatusCode, body)
	}
	var result initiateResult
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("s3: parse initiate response: %w", err)
	}
	if result.UploadID == "" {
		return nil, fmt.Errorf("s3: initiate upload %s/%s: no upload id", bucket, key)
	}
	return &Uploader{
		ctx: ctx, client: c, bucket: bucket, key: key,
		uploadID: result.UploadID, partSize: DefaultPartSize,
		buf: make([]byte, 0, DefaultPartSize),
	}, nil
}

// Write buffers data, flushing whole parts as they fill.
func (u *Uploader) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failed != nil {
		return 0, u.failed
	}
	if u.closed {
		return 0, fmt.Errorf("s3: write after close")
	}

	written := 0
	for len(p) > 0 {
		space := u.partSize - len(u.buf)
		n := min(space, len(p))
		u.buf = append(u.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(u.buf) == u.partSize {
			if err := u.flushLocked(); err != nil {
				u.failed = err
				return written, err
			}
		}
	}
	return written, nil
}

// flushLocked uploads the buffered part.
func (u *Uploader) flushLocked() error {
	if len(u.buf) == 0 {
		return nil
	}
	partNumber := len(u.parts) + 1
	etag, err := u.uploadPart(partNumber, u.buf)
	if err != nil {
		return err
	}
	u.parts = append(u.parts, completedPart{PartNumber: partNumber, ETag: etag})
	u.buf = u.buf[:0]
	return nil
}

func (u *Uploader) uploadPart(partNumber int, data []byte) (string, error) {
	base, err := url.Parse(u.client.objectURL(u.bucket, u.key))
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("partNumber", strconv.Itoa(partNumber))
	q.Set("uploadId", u.uploadID)
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(u.ctx, http.MethodPut, base.String(),
		bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(data))
	resp, err := u.client.send(req, data)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("s3: upload part %d: HTTP %d", partNumber, resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("s3: upload part %d: no ETag", partNumber)
	}
	return etag, nil
}

// Close flushes the final part and completes the upload, publishing the
// object. An upload with no data at all completes as an empty object.
func (u *Uploader) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	u.closed = true
	if u.failed != nil {
		u.abortLocked()
		return u.failed
	}
	if err := u.flushLocked(); err != nil {
		u.abortLocked()
		return err
	}
	// S3 rejects a completion with zero parts, so an empty object is
	// written as a plain PUT instead.
	if len(u.parts) == 0 {
		u.abortLocked()
		return u.client.PutObject(u.ctx, u.bucket, u.key, nil)
	}
	return u.completeLocked()
}

func (u *Uploader) completeLocked() error {
	payload, err := xml.Marshal(completeRequest{Parts: u.parts})
	if err != nil {
		return err
	}
	base, err := url.Parse(u.client.objectURL(u.bucket, u.key))
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("uploadId", u.uploadID)
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(u.ctx, http.MethodPost, base.String(),
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/xml")
	resp, err := u.client.send(req, payload)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(resp.Body)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("s3: complete upload %s/%s: HTTP %d: %s",
			u.bucket, u.key, resp.StatusCode, body)
	}
	if readErr != nil {
		return readErr
	}
	// S3 can report a failure inside a 200 response for this operation, so
	// the body has to be inspected rather than trusting the status.
	if bytes.Contains(body, []byte("<Error>")) {
		return fmt.Errorf("s3: complete upload %s/%s failed: %s", u.bucket, u.key, body)
	}
	return nil
}

// Abort discards the upload and its parts, so a failed write does not leave
// storage paying for orphaned data.
func (u *Uploader) Abort() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	u.closed = true
	u.abortLocked()
}

func (u *Uploader) abortLocked() {
	base, err := url.Parse(u.client.objectURL(u.bucket, u.key))
	if err != nil {
		return
	}
	q := url.Values{}
	q.Set("uploadId", u.uploadID)
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(context.WithoutCancel(u.ctx),
		http.MethodDelete, base.String(), nil)
	if err != nil {
		return
	}
	if resp, err := u.client.send(req, nil); err == nil {
		drain(resp)
	}
}
