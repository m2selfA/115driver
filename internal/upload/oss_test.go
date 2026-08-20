package upload

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type uploadRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn uploadRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func uploadTestHTTPResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	canonicalHeader := make(http.Header)
	for key, values := range header {
		for _, value := range values {
			canonicalHeader.Add(key, value)
		}
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: canonicalHeader,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req,
	}
}

func TestOSSBucketPoolRefreshIfStaleSingleflightAcrossInterfaces(t *testing.T) {
	path1 := transfer.NetworkPath{InterfaceName: "Ethernet 1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")}
	path2 := transfer.NetworkPath{InterfaceName: "Ethernet 2", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.1.1")}
	pool := newOSSBucketPool(nil, "http://oss.example.invalid", "bucket")
	pool.token = &driver.UploadOSSTokenResp{AccessKeyID: "old-ak", AccessKeySecret: "old-secret", SecurityToken: "old", Expiration: time.Now().Add(time.Hour)}
	pool.generation = 7
	pool.refreshed = time.Now()
	defer pool.close()
	pool.transportFactory = func(transfer.NetworkPath) (http.RoundTripper, error) {
		return uploadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("token refresh snapshot unexpectedly sent network request: %s", req.URL)
			return nil, nil
		}), nil
	}
	var refreshCalls atomic.Int32
	pool.tokenFetcher = func() (*driver.UploadOSSTokenResp, error) {
		refreshCalls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return &driver.UploadOSSTokenResp{AccessKeyID: "new-ak", AccessKeySecret: "new-secret", SecurityToken: "new", Expiration: time.Now().Add(time.Hour)}, nil
	}

	results := make(chan ossBucketSnapshot, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, path := range []transfer.NetworkPath{path1, path2} {
		path := path
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot, err := pool.refreshIfStale(path, 7)
			if err != nil {
				errs <- err
				return
			}
			results <- snapshot
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("stale STS token refreshed %d times, want 1", refreshCalls.Load())
	}
	count := 0
	for snapshot := range results {
		count++
		if snapshot.generation != 8 || snapshot.token.SecurityToken != "new" {
			t.Fatalf("unexpected refreshed snapshot: %#v", snapshot)
		}
	}
	if count != 2 {
		t.Fatalf("expected two refreshed snapshots, got %d", count)
	}
}

func TestIsOSSAuthErrorAcceptsValueAndPointerServiceErrors(t *testing.T) {
	for _, err := range []error{
		oss.ServiceError{StatusCode: http.StatusForbidden},
		&oss.ServiceError{StatusCode: http.StatusUnauthorized},
	} {
		if !isOSSAuthError(err) {
			t.Fatalf("expected OSS auth error classification for %T: %v", err, err)
		}
	}
	if isOSSAuthError(oss.ServiceError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("server failure was misclassified as an auth refresh condition")
	}
}

func TestOSSUploadPartNetworkFailureFeedsP8HealthClassification(t *testing.T) {
	path := transfer.NetworkPath{InterfaceName: "Ethernet", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")}
	pool := newOSSBucketPool(nil, "http://oss.example.invalid", "bucket")
	pool.token = &driver.UploadOSSTokenResp{AccessKeyID: "ak", AccessKeySecret: "secret", SecurityToken: "sts", Expiration: time.Now().Add(time.Hour)}
	pool.generation = 1
	pool.refreshed = time.Now()
	defer pool.close()
	pool.transportFactory = func(transfer.NetworkPath) (http.RoundTripper, error) {
		return uploadRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "write", Net: "tcp", Err: errors.New("link reset")}
		}), nil
	}
	file, err := os.CreateTemp(t.TempDir(), "p10-network-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("abcd"); err != nil {
		t.Fatal(err)
	}
	health := transfer.NewDefaultNetworkHealthTracker()
	imur := oss.InitiateMultipartUploadResult{Bucket: "bucket", Key: "object", UploadID: "upload-id"}
	report, scheduleErr := transfer.ScheduleUploadParts(context.Background(), []transfer.NetworkPath{path}, []transfer.UploadPartJob{{PartNumber: 1, Offset: 0, Size: 4}}, func(ctx context.Context, path transfer.NetworkPath, job transfer.UploadPartJob) (transfer.UploadPartResult, error) {
		return uploadPartWithRefresh(ctx, pool, imur, file, path, job)
	}, transfer.WithUploadPartRetries(0), transfer.WithUploadPartHealthTracker(health))
	if !errors.Is(scheduleErr, transfer.ErrUploadPartScheduleIncomplete) {
		t.Fatalf("expected upload schedule failure, got %v", scheduleErr)
	}
	if len(report.Results) != 1 || len(report.Results[0].Attempts) != 1 || !errors.Is(report.Results[0].Attempts[0].Err, transfer.ErrNetworkPathFailure) {
		t.Fatalf("actual OSS network error did not reach P8 classification: %#v", report.Results)
	}
	if snapshot := health.Snapshot(path); snapshot.Failures != 1 || !snapshot.InCooldown {
		t.Fatalf("actual OSS network error did not penalize path health: %#v", snapshot)
	}
}

func TestParallelMultipartUsesSameUploadIDAcrossBoundOSSClientsWhenCallbackAllowsIt(t *testing.T) {
	path1 := transfer.NetworkPath{InterfaceName: "Ethernet 1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")}
	path2 := transfer.NetworkPath{InterfaceName: "Ethernet 2", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.1.1")}
	paths := []transfer.NetworkPath{path1, path2}
	pool := newOSSBucketPool(nil, "http://oss.example.invalid", "bucket")
	pool.token = &driver.UploadOSSTokenResp{
		AccessKeyID: "ak", AccessKeySecret: "secret", SecurityToken: "sts-token", Expiration: time.Now().Add(time.Hour),
	}
	pool.generation = 1
	pool.refreshed = time.Now()
	defer pool.close()

	var initiateInterface, uploadInterface, completeInterface int
	pool.transportFactory = func(path transfer.NetworkPath) (http.RoundTripper, error) {
		interfaceIndex := path.InterfaceIndex
		return uploadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			query := req.URL.Query()
			switch {
			case req.Method == http.MethodPost && query.Has("uploads"):
				initiateInterface = interfaceIndex
				if query.Has("sequential") || query.Has("x-oss-enable-sha1") {
					t.Fatalf("parallel initiate accidentally enabled sequential SHA1: %s", req.URL.RawQuery)
				}
				return uploadTestHTTPResponse(req, http.StatusOK, http.Header{}, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>object</Key><UploadId>upload-id</UploadId></InitiateMultipartUploadResult>`), nil
			case req.Method == http.MethodPut && query.Get("uploadId") == "upload-id":
				uploadInterface = interfaceIndex
				if query.Get("partNumber") != "2" {
					t.Fatalf("unexpected part number query: %s", req.URL.RawQuery)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != "cde" {
					t.Fatalf("section reader uploaded wrong bytes: %q", body)
				}
				if req.Header.Get(driver.OssSecurityTokenHeaderName) != "sts-token" {
					t.Fatalf("missing STS token header: %#v", req.Header)
				}
				return uploadTestHTTPResponse(req, http.StatusOK, http.Header{"ETag": []string{`"etag-2"`}}, ""), nil
			case req.Method == http.MethodPost && query.Get("uploadId") == "upload-id":
				completeInterface = interfaceIndex
				encodedCallback := req.Header.Get("x-oss-callback")
				decodedCallback, err := base64.StdEncoding.DecodeString(encodedCallback)
				if err != nil {
					t.Fatalf("decode callback header: %v", err)
				}
				if string(decodedCallback) != `{"callbackBody":"name=${object}"}` {
					t.Fatalf("parallel complete mutated opaque callback: %s", decodedCallback)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				part1 := strings.Index(string(body), "<PartNumber>1</PartNumber>")
				part2 := strings.Index(string(body), "<PartNumber>2</PartNumber>")
				if part1 < 0 || part2 < 0 || part1 > part2 {
					t.Fatalf("complete request parts were not sorted: %s", body)
				}
				return uploadTestHTTPResponse(req, http.StatusOK, http.Header{}, `{"state":true,"data":{"sha1":"ABCDEF1234"}}`), nil
			default:
				t.Fatalf("unexpected OSS request on interface %d: %s %s", interfaceIndex, req.Method, req.URL.String())
				return nil, nil
			}
		}), nil
	}

	imur, err := initiateMultipart(context.Background(), pool, paths, "object", false)
	if err != nil {
		t.Fatal(err)
	}
	if imur.UploadID != "upload-id" || imur.Key != "object" {
		t.Fatalf("unexpected multipart init result: %#v", imur)
	}
	file, err := os.CreateTemp(t.TempDir(), "p10-sdk-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("abcdefgh"); err != nil {
		t.Fatal(err)
	}
	partResult, err := uploadPartWithRefresh(context.Background(), pool, imur, file, path2, transfer.UploadPartJob{PartNumber: 2, Offset: 2, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if partResult.PartNumber != 2 || partResult.ETag != `"etag-2"` || partResult.BytesUploaded != 3 {
		t.Fatalf("unexpected UploadPart result: %#v", partResult)
	}
	params := driver.UploadOSSParams{SHA1: "ABCDEF1234", Bucket: "bucket", Object: "object"}
	params.Callback.Callback = `{"callbackBody":"name=${object}"}`
	params.Callback.CallbackVar = `{"x:dir":"U_1_0"}`
	body, err := completeMultipart(context.Background(), pool, []transfer.NetworkPath{path2, path1}, imur, []oss.UploadPart{
		{PartNumber: 2, ETag: `"etag-2"`}, {PartNumber: 1, ETag: `"etag-1"`},
	}, &params, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := parseUploadCallback(body, params.SHA1); err != nil {
		t.Fatal(err)
	}
	if initiateInterface != 1 || uploadInterface != 2 || completeInterface != 2 {
		t.Fatalf("multipart phases did not cross independent bound clients as expected: init=%d upload=%d complete=%d", initiateInterface, uploadInterface, completeInterface)
	}
}

func TestSequentialMultipartInitiatePreservesLegacySHA1Flags(t *testing.T) {
	path := transfer.NetworkPath{InterfaceName: "Ethernet", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")}
	pool := newOSSBucketPool(nil, "http://oss.example.invalid", "bucket")
	pool.token = &driver.UploadOSSTokenResp{AccessKeyID: "ak", AccessKeySecret: "secret", SecurityToken: "sts", Expiration: time.Now().Add(time.Hour)}
	pool.generation = 1
	pool.refreshed = time.Now()
	defer pool.close()
	pool.transportFactory = func(transfer.NetworkPath) (http.RoundTripper, error) {
		return uploadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			query := req.URL.Query()
			if !query.Has("sequential") || !query.Has("x-oss-enable-sha1") {
				t.Fatalf("single-interface initiate lost legacy sequential SHA1 flags: %s", req.URL.RawQuery)
			}
			return uploadTestHTTPResponse(req, http.StatusOK, http.Header{}, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>object</Key><UploadId>legacy-id</UploadId></InitiateMultipartUploadResult>`), nil
		}), nil
	}
	imur, err := initiateMultipart(context.Background(), pool, []transfer.NetworkPath{path}, "object", true)
	if err != nil {
		t.Fatal(err)
	}
	if imur.UploadID != "legacy-id" {
		t.Fatalf("unexpected upload ID: %q", imur.UploadID)
	}
}

func TestUploadCallbackRequiresSequentialSHA1AndRemainsOpaque(t *testing.T) {
	params := driver.UploadOSSParams{SHA1: "ABCDEF1234", Bucket: "bucket", Object: "object"}
	params.Callback.Callback = `{"callbackBody":"sha1=${sha1}&name=${object}"}`
	params.Callback.CallbackVar = `{"x:dir":"U_1_0"}`
	if !uploadCallbackRequiresSequentialSHA1(&params) {
		t.Fatal("115 callback with ${sha1} was not classified as sequential-only")
	}
	options := DefaultOptions()
	if !requireSequentialUploadCompatibility(&options, &params) || !options.forceSequential || !options.Compatibility.SequentialRequired() {
		t.Fatalf("callback did not latch sequential compatibility: %#v", options)
	}
	for _, sequential := range []bool{false, true} {
		got := multipartCallbackParams(&params, sequential)
		if got.Callback.Callback != params.Callback.Callback || got.Callback.CallbackVar != params.Callback.CallbackVar {
			t.Fatalf("callback was mutated for sequential=%v: %#v", sequential, got.Callback)
		}
	}

	safe := params
	safe.Callback.Callback = `{"callbackBody":"name=${object}"}`
	if uploadCallbackRequiresSequentialSHA1(&safe) {
		t.Fatal("callback without ${sha1} was incorrectly forced to sequential mode")
	}
}

func TestBuildUploadPartJobsCoversFileAndHonorsOSSPartLimit(t *testing.T) {
	jobs, chunkSize, err := buildUploadPartJobs(10<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if chunkSize != 1<<20 || len(jobs) != 10 {
		t.Fatalf("unexpected normal split: chunk=%d jobs=%d", chunkSize, len(jobs))
	}
	var covered int64
	for i, job := range jobs {
		if job.PartNumber != i+1 || job.Offset != covered || job.Size <= 0 {
			t.Fatalf("invalid contiguous job %d: %#v", i, job)
		}
		covered += job.Size
	}
	if covered != 10<<20 {
		t.Fatalf("split covered %d bytes", covered)
	}

	large := int64(MaxPartCount+1) * MinPartSize
	jobs, chunkSize, err = buildUploadPartJobs(large, MinPartSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) > MaxPartCount || chunkSize <= MinPartSize {
		t.Fatalf("part limit was not enforced: jobs=%d chunk=%d", len(jobs), chunkSize)
	}
}

func TestBuildOSSProbeURLUsesBucketEndpoint(t *testing.T) {
	got, err := buildOSSProbeURL("cn-shenzhen.oss.aliyuncs.com", "example-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example-bucket.cn-shenzhen.oss.aliyuncs.com/" {
		t.Fatalf("unexpected probe URL: %q", got)
	}
	got, err = buildOSSProbeURL("http://127.0.0.1:8080/base", "bucket")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://bucket.127.0.0.1:8080/" {
		t.Fatalf("unexpected endpoint normalization: %q", got)
	}
}

func TestApplyUploadCompatibilitySelectionRetainsFailoverInterfaces(t *testing.T) {
	paths := []transfer.NetworkPath{
		{InterfaceName: "Ethernet 1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")},
		{InterfaceName: "Ethernet 2", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.1.1")},
	}
	selection := pathSelection{Paths: paths}
	parallel := applyUploadCompatibilitySelection(Options{}, selection)
	if len(parallel.Paths) != 2 {
		t.Fatalf("normal upload unexpectedly lost interfaces: %#v", parallel.Paths)
	}
	fallback := applyUploadCompatibilitySelection(Options{forceSequential: true}, selection)
	if len(fallback.Paths) != 2 || fallback.Paths[0].InterfaceIndex != 1 || fallback.Paths[1].InterfaceIndex != 2 {
		t.Fatalf("sequential compatibility lost failover candidates: %#v", fallback.Paths)
	}
	if len(selection.Paths) != 2 {
		t.Fatal("compatibility selection mutated the caller's path slice")
	}

	health := transfer.NewDefaultNetworkHealthTracker()
	health.RecordFailure(paths[0])
	healthAware := applyUploadCompatibilitySelection(Options{forceSequential: true, HealthTracker: health}, selection)
	if len(healthAware.Paths) != 2 || healthAware.Paths[0].InterfaceIndex != 2 || healthAware.Paths[1].InterfaceIndex != 1 {
		t.Fatalf("sequential failover did not rank the cooled interface last: %#v", healthAware.Paths)
	}
}

func TestSelectManualPathsMatchesNameIndexAndIP(t *testing.T) {
	paths := []transfer.NetworkPath{
		{InterfaceName: "Ethernet", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")},
		{InterfaceName: "Ethernet", InterfaceIndex: 1, LocalIP: net.ParseIP("2001:db8::1")},
		{InterfaceName: "Wi-Fi", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.1.1")},
	}
	selected, err := selectManualPaths(paths, "Ethernet,2")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 3 {
		t.Fatalf("unexpected manual selection: %#v", selected)
	}
	selected, err = selectManualPaths(paths, "10.0.1.1")
	if err != nil || len(selected) != 1 || selected[0].InterfaceIndex != 2 {
		t.Fatalf("IP selector failed: paths=%#v err=%v", selected, err)
	}
}

func TestListUploadedPartsPaginatesAndReconcilesMissingJobs(t *testing.T) {
	path := transfer.NetworkPath{InterfaceName: "Ethernet", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")}
	pool := newOSSBucketPool(nil, "http://oss.example.invalid", "bucket")
	pool.token = &driver.UploadOSSTokenResp{AccessKeyID: "ak", AccessKeySecret: "secret", SecurityToken: "sts", Expiration: time.Now().Add(time.Hour)}
	pool.generation = 1
	pool.refreshed = time.Now()
	defer pool.close()
	var calls atomic.Int32
	pool.transportFactory = func(transfer.NetworkPath) (http.RoundTripper, error) {
		return uploadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			if req.Method != http.MethodGet || req.URL.Query().Get("uploadId") != "upload-id" {
				t.Fatalf("unexpected list request: %s %s", req.Method, req.URL)
			}
			if req.Header.Get(driver.OssSecurityTokenHeaderName) != "sts" {
				t.Fatalf("missing STS token on ListUploadedParts: %#v", req.Header)
			}
			marker := req.URL.Query().Get("part-number-marker")
			switch marker {
			case "":
				return uploadTestHTTPResponse(req, http.StatusOK, http.Header{}, `<ListPartsResult><Bucket>bucket</Bucket><Key>object</Key><UploadId>upload-id</UploadId><NextPartNumberMarker>1</NextPartNumberMarker><MaxParts>1000</MaxParts><IsTruncated>true</IsTruncated><Part><PartNumber>1</PartNumber><ETag>etag-1</ETag><Size>3</Size></Part></ListPartsResult>`), nil
			case "1":
				return uploadTestHTTPResponse(req, http.StatusOK, http.Header{}, `<ListPartsResult><Bucket>bucket</Bucket><Key>object</Key><UploadId>upload-id</UploadId><NextPartNumberMarker>3</NextPartNumberMarker><MaxParts>1000</MaxParts><IsTruncated>false</IsTruncated><Part><PartNumber>3</PartNumber><ETag>etag-3</ETag><Size>2</Size></Part></ListPartsResult>`), nil
			default:
				t.Fatalf("unexpected part marker %q", marker)
				return nil, nil
			}
		}), nil
	}

	listed, err := listUploadedParts(context.Background(), pool, []transfer.NetworkPath{path}, oss.InitiateMultipartUploadResult{Bucket: "bucket", Key: "object", UploadID: "upload-id"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(listed) != 2 {
		t.Fatalf("unexpected paginated result: calls=%d parts=%#v", calls.Load(), listed)
	}
	jobs := []transfer.UploadPartJob{
		{PartNumber: 1, Offset: 0, Size: 3},
		{PartNumber: 2, Offset: 3, Size: 3},
		{PartNumber: 3, Offset: 6, Size: 2},
	}
	existing, missing, err := reconcileUploadedParts(jobs, listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(existing) != 2 || existing[0].PartNumber != 1 || existing[1].PartNumber != 3 {
		t.Fatalf("unexpected existing parts: %#v", existing)
	}
	if len(missing) != 1 || missing[0].PartNumber != 2 {
		t.Fatalf("resume did not isolate the missing part: %#v", missing)
	}
}

func TestReconcileUploadedPartsRejectsMismatchedRemoteLayout(t *testing.T) {
	jobs := []transfer.UploadPartJob{{PartNumber: 1, Offset: 0, Size: 4}}
	_, _, err := reconcileUploadedParts(jobs, []listedUploadPart{{Part: oss.UploadPart{PartNumber: 1, ETag: "etag"}, Size: 3}})
	if !errors.Is(err, ErrUploadResumeState) {
		t.Fatalf("expected resume layout error, got %v", err)
	}
}

func TestIsOSSNoSuchUploadAcceptsValueAndPointer(t *testing.T) {
	for _, err := range []error{
		oss.ServiceError{Code: "NoSuchUpload", StatusCode: http.StatusNotFound},
		&oss.ServiceError{Code: "NoSuchUpload", StatusCode: http.StatusNotFound},
	} {
		if !isOSSNoSuchUpload(err) {
			t.Fatalf("expected NoSuchUpload classification for %T", err)
		}
	}
	if isOSSNoSuchUpload(oss.ServiceError{Code: "AccessDenied", StatusCode: http.StatusForbidden}) {
		t.Fatal("unrelated OSS error was classified as NoSuchUpload")
	}
}

func TestParseUploadCallbackRejectsSHA1Mismatch(t *testing.T) {
	body := []byte(`{"state":true,"data":{"sha1":"WRONG"}}`)
	if err := parseUploadCallback(body, "EXPECTED"); err == nil {
		t.Fatal("expected callback SHA1 mismatch")
	}
}
