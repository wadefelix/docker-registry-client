package registry

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"

	"github.com/docker/distribution"
	digest "github.com/opencontainers/go-digest"
)

func (registry *Registry) DownloadBlob(repository string, digest digest.Digest) (io.ReadCloser, error) {
	url := registry.url("/v2/%s/blobs/%s", repository, digest)
	registry.Logf("registry.blob.download url=%s repository=%s digest=%s", url, repository, digest)

	resp, err := registry.Client.Get(url)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

func (registry *Registry) GetBlobContent(repository string, digest digest.Digest) ([]byte, error) {
	url := registry.url("/v2/%s/blobs/%s", repository, digest)
	registry.Logf("registry.blob.download url=%s repository=%s digest=%s", url, repository, digest)

	resp, err := registry.Client.Get(url)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// UploadBlob can be used to upload an FS layer or an image config file into the given repository.
// It uploads the bytes read from content. Digest must match with the hash of those bytes.
// In case of token authentication the HTTP request must be retried after a 401 Unauthorized response
// (see https://docs.docker.com/registry/spec/auth/token/). In this case the getBody function is called
// in order to retrieve a fresh instance of the content reader. This behaviour matches exactly of the
// GetBody parameter of http.Client. This also means that if content is of type *bytes.Buffer,
// *bytes.Reader or *strings.Reader, then GetBody is populated automatically (as explained in the
// documentation of http.NewRequest()), so nil can be passed as the getBody parameter.
func (registry *Registry) UploadBlob(repository string, digest digest.Digest, content io.Reader, getBody func() (io.ReadCloser, error)) error {
	uploadUrl, err := registry.initiateUpload(repository)
	if err != nil {
		return err
	}
	q := uploadUrl.Query()
	q.Set("digest", digest.String())
	uploadUrl.RawQuery = q.Encode()

	registry.Logf("registry.blob.upload url=%s repository=%s digest=%s", uploadUrl, repository, digest)

	upload, err := http.NewRequest("PUT", uploadUrl.String(), content)
	if err != nil {
		return err
	}
	upload.Header.Set("Content-Type", "application/octet-stream")
	if getBody != nil {
		upload.GetBody = getBody
	}

	resp, err := registry.Client.Do(upload)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
func (registry *Registry) UploadBlobChunked(repository string, digest digest.Digest, contBytes []byte) error {
	chunkSize := 8096000
	contLength := len(contBytes)

	if contLength <= chunkSize {
		return registry.UploadBlob(repository, digest, bytes.NewBuffer(contBytes), nil)
	}

	uploadUrl, err := registry.initiateUpload(repository)
	if err != nil {
		return err
	}

	chunk := contLength / chunkSize
	lastChSize := contLength % chunkSize

	for ch := 0; ch < chunk; ch++ {
		rangeStart := ch * chunkSize
		rangeEnd := rangeStart + chunkSize
		content := bytes.NewBuffer(contBytes[rangeStart:rangeEnd])
		upload, err := http.NewRequest("PATCH", uploadUrl.String(), content)
		if err != nil {
			return err
		}
		upload.Header.Set("Content-Type", "application/octet-stream")
		upload.Header.Set("Content-Length", strconv.Itoa(chunkSize))
		contRange := fmt.Sprintf("%d-%d", rangeStart, rangeEnd-1)
		upload.Header.Set("Content-Range", contRange)
		registry.Logf("registry.blob.upload url=%s Content-Range=%s", uploadUrl, contRange)

		resp, err := registry.Client.Do(upload)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
	}

	q := uploadUrl.Query()
	q.Set("digest", digest.String())
	uploadUrl.RawQuery = q.Encode()

	rangeStart := chunk * chunkSize
	rangeEnd := contLength
	content := bytes.NewBuffer(contBytes[rangeStart:rangeEnd])
	upload, err := http.NewRequest("PUT", uploadUrl.String(), content)
	if err != nil {
		return err
	}
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set("Content-Length", strconv.Itoa(lastChSize))
	contRange := fmt.Sprintf("%d-%d", rangeStart, rangeEnd-1)
	upload.Header.Set("Content-Range", contRange)

	registry.Logf("registry.blob.upload url=%s Content-Range=%s repository=%s digest=%s", uploadUrl, contRange, repository, digest)
	resp, err := registry.Client.Do(upload)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (registry *Registry) MountBlob(repository string, digest digest.Digest, fromrepo string) error {
	url := registry.url("/v2/%s/blobs/uploads/?mount=%s&from=%s", repository, digest, fromrepo)
	registry.Logf("registry.blob.mount url=%s repository=%s digest=%s from=%s", url, repository, digest, fromrepo)

	resp, err := registry.Client.Post(url, "application/octet-stream", nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}

	return nil
}

func (registry *Registry) HasBlob(repository string, digest digest.Digest) (bool, error) {
	checkUrl := registry.url("/v2/%s/blobs/%s", repository, digest)
	registry.Logf("registry.blob.check url=%s repository=%s digest=%s", checkUrl, repository, digest)

	resp, err := registry.Client.Head(checkUrl)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		return resp.StatusCode == http.StatusOK, nil
	}

	urlErr, ok := err.(*url.Error)
	if !ok {
		return false, err
	}
	httpErr, ok := urlErr.Err.(*HttpStatusError)
	if !ok {
		return false, err
	}
	if httpErr.Response.StatusCode == http.StatusNotFound {
		return false, nil
	}

	return false, err
}

func (registry *Registry) BlobMetadata(repository string, digest digest.Digest) (distribution.Descriptor, error) {
	checkUrl := registry.url("/v2/%s/blobs/%s", repository, digest)
	registry.Logf("registry.blob.check url=%s repository=%s digest=%s", checkUrl, repository, digest)

	resp, err := registry.Client.Head(checkUrl)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return distribution.Descriptor{}, err
	}

	return distribution.Descriptor{
		Digest: digest,
		Size:   resp.ContentLength,
	}, nil
}

func (registry *Registry) initiateUpload(repository string) (*url.URL, error) {
	initiateUrl := registry.url("/v2/%s/blobs/uploads/", repository)
	registry.Logf("registry.blob.initiate-upload url=%s repository=%s", initiateUrl, repository)

	resp, err := registry.Client.Post(initiateUrl, "application/octet-stream", nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}

	location := resp.Header.Get("Location")
	locationUrl, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	return locationUrl, nil
}
