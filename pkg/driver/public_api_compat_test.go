package driver_test

import "github.com/SheltonZhu/115driver/pkg/driver"

// These unkeyed literals intentionally pin the v0.1.4 public layouts that R16
// restored after robustness work had temporarily changed value fields to
// pointers. They catch source-breaking field additions, removals, reordering,
// or type changes in these compatibility-sensitive response structs.
var _ = driver.QRCodeStatusResp{
	driver.QRCodeBasicResp{},
	driver.QRCodeStatus{},
}

var _ = driver.OfflineTaskResponse{"", ""}

var _ = func() driver.SharedDownloadInfo {
	var zero driver.SharedDownloadInfo
	return driver.SharedDownloadInfo{"", "", driver.StringInt64(0), zero.URL}
}()

// Keyed ListOptions literals remain source-compatible. The one documented
// v0.2 exception is an external unkeyed literal, because record-open-time state
// is intentionally private.
var _ = driver.ListOptions{ApiURLs: []string{"https://example.invalid"}}

var _ = driver.FileStatResponse{
	driver.StringInt(0),
	"",
	driver.StringInt(0),
	driver.StringInt64(0),
	driver.StringInt64(0),
	driver.StringInt(0),
	"",
	"",
	"",
	driver.StringInt(0),
	int64(0),
	driver.StringInt(0),
	nil,
}
