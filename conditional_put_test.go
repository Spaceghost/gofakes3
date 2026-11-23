package gofakes3_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3afero"
	"github.com/johannesboyne/gofakes3/backend/s3bolt"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// backendTestCase represents a backend configuration for testing
type backendTestCase struct {
	name    string
	backend gofakes3.Backend
	cleanup func() // Optional cleanup function
}

// createAllBackends creates instances of all available backends for testing
func createAllBackends(t *testing.T) []backendTestCase {
	t.Helper()

	var backends []backendTestCase

	// 1. s3mem backend
	backends = append(backends, backendTestCase{
		name:    "s3mem",
		backend: s3mem.New(s3mem.WithTimeSource(gofakes3.FixedTimeSource(defaultDate))),
		cleanup: nil,
	})

	// 2. s3bolt backend
	tempBoltFile, err := ioutil.TempFile("", "gofakes3-test-*.db")
	if err != nil {
		t.Fatal("Failed to create temp bolt file:", err)
	}
	tempBoltFile.Close()

	boltDB, err := bolt.Open(tempBoltFile.Name(), 0600, nil)
	if err != nil {
		os.Remove(tempBoltFile.Name())
		t.Fatal("Failed to open bolt database:", err)
	}

	boltBackend := s3bolt.New(boltDB, s3bolt.WithTimeSource(gofakes3.FixedTimeSource(defaultDate)))

	backends = append(backends, backendTestCase{
		name:    "s3bolt",
		backend: boltBackend,
		cleanup: func() {
			boltDB.Close()
			os.Remove(tempBoltFile.Name())
		},
	})

	// 3. s3afero SingleBucket backend (use defaultBucket name to match test expectations)
	singleBackend, err := s3afero.SingleBucket(defaultBucket, afero.NewMemMapFs(), nil)
	if err != nil {
		t.Fatal("Failed to create s3afero single backend:", err)
	}

	backends = append(backends, backendTestCase{
		name:    "s3afero-single",
		backend: singleBackend,
		cleanup: nil,
	})

	// 4. s3afero MultiBucket backend
	multiBackend, err := s3afero.MultiBucket(afero.NewMemMapFs())
	if err != nil {
		t.Fatal("Failed to create s3afero multi backend:", err)
	}

	backends = append(backends, backendTestCase{
		name:    "s3afero-multi",
		backend: multiBackend,
		cleanup: nil,
	})

	return backends
}

// runWithAllBackends runs a test function against all backend implementations
func runWithAllBackends(t *testing.T, testFunc func(*testing.T, *testServer)) {
	backends := createAllBackends(t)

	for _, backendCase := range backends {
		t.Run(backendCase.name, func(t *testing.T) {
			// Ensure cleanup happens even if test fails
			if backendCase.cleanup != nil {
				defer backendCase.cleanup()
			}

			// Create test server with this backend
			var ts *testServer
			if backendCase.name == "s3afero-single" {
				// SingleBucket backend already has the bucket, don't try to create it
				ts = newTestServer(t, withBackend(backendCase.backend), withoutInitialBuckets())
			} else {
				ts = newTestServer(t, withBackend(backendCase.backend))
			}
			defer ts.Close()

			// Run the test function
			testFunc(t, ts)
		})
	}
}

func TestConditionalPutIfNoneMatch(t *testing.T) {
	runWithAllBackends(t, testConditionalPutIfNoneMatch)
}

func testConditionalPutIfNoneMatch(t *testing.T, ts *testServer) {
	svc := ts.s3Client()

	bucket := aws.String(defaultBucket)
	key := aws.String("test-object")
	body := []byte("test content")

	// Test 1: If-None-Match with * should succeed when object doesn't exist
	_, err := svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      bucket,
		Key:         key,
		Body:        bytes.NewReader(body),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		t.Fatal("Expected success when object doesn't exist:", err)
	}

	// Test 2: If-None-Match with * should fail when object exists
	_, err = svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      bucket,
		Key:         key,
		Body:        bytes.NewReader([]byte("new content")),
		IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		t.Fatal("Expected failure when object exists")
	}
	if !hasErrorCode(err, gofakes3.ErrPreconditionFailed) {
		t.Fatal("Expected PreconditionFailed error, got:", err)
	}

	// Verify original content is unchanged
	obj := ts.backendGetString(defaultBucket, "test-object", nil)
	if obj != "test content" {
		t.Fatal("Object content was modified when it shouldn't have been")
	}
}

func TestConditionalPutIfMatch(t *testing.T) {
	runWithAllBackends(t, testConditionalPutIfMatch)
}

func testConditionalPutIfMatch(t *testing.T, ts *testServer) {
	svc := ts.s3Client()

	bucket := aws.String(defaultBucket)
	key := aws.String("test-object")
	body := []byte("test content")

	// Create initial object
	putResp, err := svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: bucket,
		Key:    key,
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatal("Failed to create initial object:", err)
	}
	etag := *putResp.ETag

	// Test 1: If-Match with correct ETag should succeed
	_, err = svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:  bucket,
		Key:     key,
		Body:    bytes.NewReader([]byte("updated content")),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		t.Fatal("Expected success with matching ETag:", err)
	}

	// Test 2: If-Match with wrong ETag should fail
	_, err = svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:  bucket,
		Key:     key,
		Body:    bytes.NewReader([]byte("another update")),
		IfMatch: aws.String(`"wrong-etag"`),
	})
	if err == nil {
		t.Fatal("Expected failure with wrong ETag")
	}
	if !hasErrorCode(err, gofakes3.ErrPreconditionFailed) {
		t.Fatal("Expected PreconditionFailed error, got:", err)
	}

	// Verify content is the updated content, not the failed update
	obj := ts.backendGetString(defaultBucket, "test-object", nil)
	if obj != "updated content" {
		t.Fatal("Object content is not the expected updated content")
	}
}

func TestConditionalPutNonExistentObject(t *testing.T) {
	runWithAllBackends(t, testConditionalPutNonExistentObject)
}

func testConditionalPutNonExistentObject(t *testing.T, ts *testServer) {
	svc := ts.s3Client()

	bucket := aws.String(defaultBucket)
	key := aws.String("nonexistent-object")

	// Test: If-Match on non-existent object should fail
	_, err := svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:  bucket,
		Key:     key,
		Body:    bytes.NewReader([]byte("content")),
		IfMatch: aws.String(`"some-etag"`),
	})
	if err == nil {
		t.Fatal("Expected failure when object doesn't exist")
	}
	if !hasErrorCode(err, gofakes3.ErrPreconditionFailed) {
		t.Fatal("Expected PreconditionFailed error, got:", err)
	}

	// Verify object was not created
	if exists, _ := ts.backendObjectExists(defaultBucket, "nonexistent-object"); exists {
		t.Fatal("Object should not have been created")
	}
}

func TestConditionalPutMultipleConditions(t *testing.T) {
	runWithAllBackends(t, testConditionalPutMultipleConditions)
}

func testConditionalPutMultipleConditions(t *testing.T, ts *testServer) {
	svc := ts.s3Client()

	bucket := aws.String(defaultBucket)
	key := aws.String("test-object")
	body := []byte("test content")

	// Create initial object
	putResp, err := svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: bucket,
		Key:    key,
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatal("Failed to create initial object:", err)
	}
	etag := *putResp.ETag

	// Test: If-Match condition should pass
	_, err = svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:  bucket,
		Key:     key,
		Body:    bytes.NewReader([]byte("updated content")),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		t.Fatal("Expected success with If-Match condition:", err)
	}

	// Verify content was updated
	obj := ts.backendGetString(defaultBucket, "test-object", nil)
	if obj != "updated content" {
		t.Fatal("Object content was not updated")
	}
}

func TestConditionalPutCopyOperation(t *testing.T) {
	runWithAllBackends(t, testConditionalPutCopyOperation)
}

func testConditionalPutCopyOperation(t *testing.T, ts *testServer) {
	svc := ts.s3Client()

	bucket := aws.String(defaultBucket)
	sourceKey := aws.String("source-object")
	destKey := aws.String("dest-object")

	// Create source object
	_, err := svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: bucket,
		Key:    sourceKey,
		Body:   bytes.NewReader([]byte("source content")),
	})
	if err != nil {
		t.Fatal("Failed to create source object:", err)
	}

	_, err = svc.CopyObject(context.TODO(), &s3.CopyObjectInput{
		Bucket: bucket, Key: destKey, CopySource: aws.String(defaultBucket + "/" + *sourceKey),
		CopySourceIfMatch: aws.String(`"wrong"`), IfNoneMatch: aws.String("*"),
	})
	if !hasErrorCode(err, gofakes3.ErrPreconditionFailed) {
		t.Fatalf("copy with stale source ETag: %v", err)
	}

	// Create the destination with a conditional copy.
	_, err = svc.CopyObject(context.TODO(), &s3.CopyObjectInput{
		Bucket:      bucket,
		Key:         destKey,
		CopySource:  aws.String(defaultBucket + "/" + *sourceKey),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		t.Fatal("Copy operation failed:", err)
	}

	_, err = svc.CopyObject(context.TODO(), &s3.CopyObjectInput{
		Bucket: bucket, Key: destKey, CopySource: aws.String(defaultBucket + "/" + *sourceKey), IfNoneMatch: aws.String("*"),
	})
	if !hasErrorCode(err, gofakes3.ErrPreconditionFailed) {
		t.Fatalf("copy to existing destination: %v", err)
	}

	// Verify copy succeeded
	obj := ts.backendGetString(defaultBucket, "dest-object", nil)
	if obj != "source content" {
		t.Fatal("Copy operation did not work correctly")
	}
}

func TestConditionalPutETagComparison(t *testing.T) {
	runWithAllBackends(t, testConditionalPutETagComparison)
}

func testConditionalPutETagComparison(t *testing.T, ts *testServer) {
	svc := ts.s3Client()

	bucket := aws.String(defaultBucket)
	key := aws.String("test-object")
	body := []byte("test content")

	// Create initial object
	putResp, err := svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: bucket,
		Key:    key,
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatal("Failed to create initial object:", err)
	}
	etag := *putResp.ETag

	// Test 1: ETag with quotes should work
	_, err = svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:  bucket,
		Key:     key,
		Body:    bytes.NewReader([]byte("updated content 1")),
		IfMatch: aws.String(etag), // ETag already has quotes
	})
	if err != nil {
		t.Fatal("Expected success with quoted ETag:", err)
	}

	// Get new ETag
	getResp, err := svc.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		t.Fatal("Failed to get object:", err)
	}
	newEtag := *getResp.ETag

	// Test 2: ETag without quotes should also work
	unquotedEtag := newEtag[1 : len(newEtag)-1] // Remove quotes
	_, err = svc.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:  bucket,
		Key:     key,
		Body:    bytes.NewReader([]byte("updated content 2")),
		IfMatch: aws.String(unquotedEtag),
	})
	if err != nil {
		t.Fatal("Expected success with unquoted ETag:", err)
	}
}

func TestConditionalMultipartCompletion(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	const key = "multipart-destination"
	id := ts.createMultipartUpload(defaultBucket, key, nil)
	part := ts.uploadPart(defaultBucket, key, id, 1, []byte("replacement"))
	svc := ts.s3Client()
	existing, err := svc.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String(defaultBucket), Key: aws.String(key), Body: bytes.NewReader([]byte("original"))})
	ts.OK(err)
	in := &s3.CompleteMultipartUploadInput{Bucket: aws.String(defaultBucket), Key: aws.String(key), UploadId: aws.String(id), IfNoneMatch: aws.String("*"), MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{part}}}
	_, err = svc.CompleteMultipartUpload(context.Background(), in)
	if !hasErrorCode(err, gofakes3.ErrPreconditionFailed) {
		t.Fatalf("completion over existing object: %v", err)
	}
	if got := ts.backendGetString(defaultBucket, key, nil); got != "original" {
		t.Fatalf("destination after failed completion: %q", got)
	}
	in.IfNoneMatch = nil
	in.IfMatch = existing.ETag
	_, err = svc.CompleteMultipartUpload(context.Background(), in)
	ts.OK(err)
}

type conditionalWriteTestBackend struct {
	gofakes3.Backend
	put  func(string, string, io.Reader, int64, *gofakes3.WriteConditions) (gofakes3.PutObjectResult, error)
	copy func(string, string, string, string, *gofakes3.WriteConditions) (gofakes3.CopyObjectResult, error)
}

func (b conditionalWriteTestBackend) PutObjectConditional(bucket, key string, _ map[string]string, input io.Reader, size int64, conditions *gofakes3.WriteConditions) (gofakes3.PutObjectResult, error) {
	return b.put(bucket, key, input, size, conditions)
}

func (b conditionalWriteTestBackend) CopyObjectConditional(srcBucket, srcKey, dstBucket, dstKey string, _ map[string]string, conditions *gofakes3.WriteConditions) (gofakes3.CopyObjectResult, error) {
	return b.copy(srcBucket, srcKey, dstBucket, dstKey, conditions)
}

func TestConditionalWriteBackendDispatch(t *testing.T) {
	const data = "replacement"
	const destination = "destination"
	path := "/" + defaultBucket + "/" + destination

	for _, operation := range []string{"Put", "Copy", "Multipart"} {
		t.Run(operation, func(t *testing.T) {
			base := s3mem.New()
			require.NoError(t, base.CreateBucket(defaultBucket))
			_, err := base.PutObject(defaultBucket, "source", nil, strings.NewReader(data), int64(len(data)), nil)
			require.NoError(t, err)

			calls := 0
			checkDestination := func(bucket, key string, conditions *gofakes3.WriteConditions) {
				calls++
				require.Equal(t, defaultBucket, bucket)
				require.Equal(t, destination, key)
				require.NoError(t, conditions.Check(false, ""))
				require.Equal(t, gofakes3.ErrPreconditionFailed, conditions.Check(true, `"etag"`))
			}
			backend := conditionalWriteTestBackend{
				Backend: base,
				put: func(bucket, key string, input io.Reader, size int64, conditions *gofakes3.WriteConditions) (gofakes3.PutObjectResult, error) {
					require.NotEqual(t, "Copy", operation)
					checkDestination(bucket, key, conditions)
					require.EqualValues(t, len(data), size)
					body, err := io.ReadAll(input)
					require.NoError(t, err)
					require.Equal(t, data, string(body))
					return gofakes3.PutObjectResult{}, gofakes3.ErrConditionalRequestConflict
				},
				copy: func(srcBucket, srcKey, dstBucket, dstKey string, conditions *gofakes3.WriteConditions) (gofakes3.CopyObjectResult, error) {
					require.Equal(t, "Copy", operation)
					checkDestination(dstBucket, dstKey, conditions)
					require.Equal(t, defaultBucket, srcBucket)
					require.Equal(t, "source", srcKey)
					return gofakes3.CopyObjectResult{}, gofakes3.ErrConditionalRequestConflict
				},
			}
			handler := gofakes3.New(backend).Server()

			var req *http.Request
			switch operation {
			case "Put":
				req = httptest.NewRequest(http.MethodPut, path, strings.NewReader(data))
				req.Header.Set("Content-Length", strconv.Itoa(len(data)))
			case "Copy":
				req = httptest.NewRequest(http.MethodPut, path, nil)
				req.Header.Set("X-Amz-Copy-Source", "/"+defaultBucket+"/source")
			case "Multipart":
				req = multipartCompletionRequest(t, handler, path, data)
			}
			req.Header.Set("If-None-Match", "*")
			assertConditionalResponse(t, handler, req, http.StatusConflict, gofakes3.ErrConditionalRequestConflict)
			require.Equal(t, 1, calls)
			_, err = base.HeadObject(defaultBucket, destination)
			require.True(t, gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchKey), "expected no destination, got %v", err)
		})
	}
}

func multipartCompletionRequest(t *testing.T, handler http.Handler, path, data string) *http.Request {
	t.Helper()
	init := httptest.NewRecorder()
	handler.ServeHTTP(init, httptest.NewRequest(http.MethodPost, path+"?uploads", nil))
	require.Equal(t, http.StatusOK, init.Code, init.Body.String())
	var upload gofakes3.InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(init.Body.Bytes(), &upload))

	part := httptest.NewRecorder()
	partReq := httptest.NewRequest(http.MethodPut, path+"?partNumber=1&uploadId="+string(upload.UploadID), strings.NewReader(data))
	partReq.Header.Set("Content-Length", strconv.Itoa(len(data)))
	handler.ServeHTTP(part, partReq)
	require.Equal(t, http.StatusOK, part.Code, part.Body.String())
	body, err := xml.Marshal(gofakes3.CompleteMultipartUploadRequest{
		Parts: []gofakes3.CompletedPart{{PartNumber: 1, ETag: part.Header().Get("ETag")}},
	})
	require.NoError(t, err)

	return httptest.NewRequest(http.MethodPost, path+"?uploadId="+string(upload.UploadID), bytes.NewReader(body))
}

type legacyMultipartTestBackend struct {
	gofakes3.Backend
	gofakes3.MultipartBackend
	headErr  error
	complete func(string, string, gofakes3.UploadID, *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error)
}

func (b legacyMultipartTestBackend) HeadObject(bucket, key string) (*gofakes3.Object, error) {
	if b.headErr != nil {
		return nil, b.headErr
	}
	return b.Backend.HeadObject(bucket, key)
}

func (b legacyMultipartTestBackend) CompleteMultipartUpload(bucket, key string, id gofakes3.UploadID, input *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	return b.complete(bucket, key, id, input)
}

func TestLegacyMultipartWriteConditions(t *testing.T) {
	for _, test := range []struct {
		name    string
		match   string
		headErr gofakes3.ErrorCode
		status  int
		code    gofakes3.ErrorCode
	}{
		{name: "Malformed", match: `"unterminated`, headErr: gofakes3.ErrInternal, status: http.StatusBadRequest, code: gofakes3.ErrInvalidArgument},
		{name: "FailedPrecondition", match: `"stale"`, status: http.StatusPreconditionFailed, code: gofakes3.ErrPreconditionFailed},
		{name: "DestinationError", match: "*", headErr: gofakes3.ErrInternal, status: http.StatusInternalServerError, code: gofakes3.ErrInternal},
		{name: "MatchingDestination", match: "*", status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := s3mem.New()
			require.NoError(t, base.CreateBucket(defaultBucket))
			const key = "destination"
			_, err := base.PutObject(defaultBucket, key, nil, strings.NewReader("data"), 4, nil)
			require.NoError(t, err)
			backend := legacyMultipartTestBackend{
				Backend: base,
				complete: func(bucket, object string, id gofakes3.UploadID, input *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
					require.Equal(t, http.StatusOK, test.status, "legacy backend must only complete a valid request")
					require.Equal(t, defaultBucket, bucket)
					require.Equal(t, key, object)
					require.Equal(t, gofakes3.UploadID("upload"), id)
					require.Equal(t, []gofakes3.CompletedPart{{PartNumber: 1, ETag: "etag"}}, input.Parts)
					return "version", `"etag"`, nil
				},
			}
			if test.headErr != "" {
				backend.headErr = test.headErr
			}
			path := "/" + defaultBucket + "/" + key + "?uploadId=upload"
			body := strings.NewReader("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>etag</ETag></Part></CompleteMultipartUpload>")
			req := httptest.NewRequest(http.MethodPost, path, body)
			req.Header.Set("If-Match", test.match)
			assertConditionalResponse(t, gofakes3.New(backend).Server(), req, test.status, test.code)
		})
	}
}

func assertConditionalResponse(t *testing.T, handler http.Handler, req *http.Request, status int, code gofakes3.ErrorCode) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, status, rec.Code, rec.Body.String())
	if code != "" {
		var response gofakes3.ErrorResponse
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &response))
		require.Equal(t, code, response.Code)
	}
}
