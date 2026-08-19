package transfer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleUploadPartsUsesOneWorkerPerInterfaceAndSharedQueue(t *testing.T) {
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	jobs := []UploadPartJob{
		{PartNumber: 1, Offset: 0, Size: 4},
		{PartNumber: 2, Offset: 4, Size: 4},
		{PartNumber: 3, Offset: 8, Size: 4},
		{PartNumber: 4, Offset: 12, Size: 4},
	}
	var mu sync.Mutex
	activeByInterface := map[int]int{}
	maxByInterface := map[int]int{}
	globalActive := 0
	maxGlobal := 0

	report, err := ScheduleUploadParts(context.Background(), paths, jobs, func(_ context.Context, path NetworkPath, job UploadPartJob) (UploadPartResult, error) {
		mu.Lock()
		activeByInterface[path.InterfaceIndex]++
		if activeByInterface[path.InterfaceIndex] > maxByInterface[path.InterfaceIndex] {
			maxByInterface[path.InterfaceIndex] = activeByInterface[path.InterfaceIndex]
		}
		globalActive++
		if globalActive > maxGlobal {
			maxGlobal = globalActive
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		activeByInterface[path.InterfaceIndex]--
		globalActive--
		mu.Unlock()
		return UploadPartResult{PartNumber: job.PartNumber, ETag: fmt.Sprintf("etag-%d", job.PartNumber), BytesUploaded: job.Size}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.SucceededCount() != len(jobs) || report.FailedCount() != 0 {
		t.Fatalf("unexpected report counts: %#v", report)
	}
	for i, result := range report.Results {
		if result.Job.PartNumber != jobs[i].PartNumber || result.Result.ETag != fmt.Sprintf("etag-%d", jobs[i].PartNumber) {
			t.Fatalf("result order changed: %#v", report.Results)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxGlobal != 2 {
		t.Fatalf("expected both interfaces to upload concurrently, max=%d", maxGlobal)
	}
	for _, path := range paths {
		if maxByInterface[path.InterfaceIndex] != 1 {
			t.Fatalf("interface %d had %d concurrent uploads", path.InterfaceIndex, maxByInterface[path.InterfaceIndex])
		}
	}
}

func TestScheduleUploadPartsRetriesNetworkFailureOnDifferentInterface(t *testing.T) {
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	var failed atomic.Bool
	report, err := ScheduleUploadParts(context.Background(), paths, []UploadPartJob{{PartNumber: 1, Offset: 0, Size: 4}}, func(_ context.Context, path NetworkPath, job UploadPartJob) (UploadPartResult, error) {
		if path.InterfaceIndex == 1 && failed.CompareAndSwap(false, true) {
			return UploadPartResult{}, &net.OpError{Op: "write", Net: "tcp", Err: errors.New("link reset")}
		}
		return UploadPartResult{ETag: "ok", BytesUploaded: job.Size}, nil
	}, WithUploadPartRetries(1))
	if err != nil {
		t.Fatal(err)
	}
	attempts := report.Results[0].Attempts
	if len(attempts) != 2 {
		t.Fatalf("expected retry, attempts=%#v", attempts)
	}
	if attempts[0].NetworkPath.InterfaceIndex == attempts[1].NetworkPath.InterfaceIndex {
		t.Fatalf("retry did not switch physical interface: %#v", attempts)
	}
	if !errors.Is(attempts[0].Err, ErrNetworkPathFailure) {
		t.Fatalf("network error was not classified for P8: %v", attempts[0].Err)
	}
}

func TestScheduleUploadPartsCancellationStopsPendingWork(t *testing.T) {
	path := testNetworkPath(1, "10.0.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := ScheduleUploadParts(ctx, []NetworkPath{path}, []UploadPartJob{
			{PartNumber: 1, Offset: 0, Size: 4},
			{PartNumber: 2, Offset: 4, Size: 4},
			{PartNumber: 3, Offset: 8, Size: 4},
		}, func(ctx context.Context, _ NetworkPath, _ UploadPartJob) (UploadPartResult, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-ctx.Done()
			return UploadPartResult{}, ctx.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUploadPartScheduleIncomplete) {
			t.Fatalf("unexpected cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("upload scheduler did not exit after cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("pending parts were dispatched after cancellation: calls=%d", calls.Load())
	}
}

func TestValidateUploadPartScheduleRejectsUnsafeJobs(t *testing.T) {
	path := testNetworkPath(1, "10.0.0.1")
	upload := func(context.Context, NetworkPath, UploadPartJob) (UploadPartResult, error) {
		return UploadPartResult{}, nil
	}
	for _, jobs := range [][]UploadPartJob{
		{{PartNumber: 0, Offset: 0, Size: 1}},
		{{PartNumber: 10001, Offset: 0, Size: 1}},
		{{PartNumber: 1, Offset: -1, Size: 1}},
		{{PartNumber: 1, Offset: 0, Size: 0}},
		{{PartNumber: 1, Offset: 0, Size: 1}, {PartNumber: 1, Offset: 1, Size: 1}},
	} {
		if _, err := ScheduleUploadParts(context.Background(), []NetworkPath{path}, jobs, upload); err == nil {
			t.Fatalf("expected invalid jobs to fail: %#v", jobs)
		}
	}
}
