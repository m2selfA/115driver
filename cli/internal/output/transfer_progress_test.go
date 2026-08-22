package output

import (
	"strings"
	"testing"

	"github.com/cheggaaa/pb/v3"
)

func TestTransferProgressUsesHumanReadableAdaptiveLine(t *testing.T) {
	const (
		completed = int64(456824522)
		total     = int64(146408155716)
	)
	status := `[4/1475] data\map_10A\HAADF_iDPC_depth_section_5mrad__0.7636nm_-2.7um_name.emd`
	bar := pb.New64(total).SetWidth(96)
	configureTransferProgressBar(bar, status, completed, total)
	if !bar.GetBool(pb.Bytes) {
		t.Fatal("transfer progress bar must format speed as bytes")
	}
	bar.SetCurrent(completed)

	rendered := bar.String()
	if width := pb.CellCount(rendered); width > 96 {
		t.Fatalf("progress line width=%d exceeds terminal width: %q", width, rendered)
	}
	for _, want := range []string{"[4/1475]", ".emd", "435.7MiB/136.4GiB", "%", "/s"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("progress line %q does not contain %q", rendered, want)
		}
	}
	for _, unwanted := range []string{"456824522", "146408155716", "p/s", "[]"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("progress line still contains raw/noisy token %q: %q", unwanted, rendered)
		}
	}
}

func TestCompactTransferProgressStatusPreservesDirectoryAndFilenameTail(t *testing.T) {
	status := `[12/1475] data\experiment_with_a_very_long_name\section_2026\map_10A\HAADF_iDPC_depth_section_5mrad__0.7636nm_-2.7um_name.emd`
	got := compactTransferProgressStatusWidth(status, 54)
	if width := pb.CellCount(got); width > 54 {
		t.Fatalf("compacted status width=%d > 54: %q", width, got)
	}
	for _, want := range []string{"[12/1475]", "data", "map_10A", ".emd", "…"} {
		if !strings.Contains(got, want) {
			t.Fatalf("compacted status %q does not preserve %q", got, want)
		}
	}
}

func TestCompactTransferProgressStatusKeepsRecoveryContextWhenSpaceAllows(t *testing.T) {
	status := `[2/42] data\map_10A\very_long_scientific_filename_with_parameters_0013.emd — Recovering upload; retry 2/3 after: connection reset by peer`
	got := compactTransferProgressStatusWidth(status, 80)
	if width := pb.CellCount(got); width > 80 {
		t.Fatalf("compacted status width=%d > 80: %q", width, got)
	}
	for _, want := range []string{"[2/42]", ".emd", "Recovering upload", "retry"} {
		if !strings.Contains(got, want) {
			t.Fatalf("compacted recovery status %q does not preserve %q", got, want)
		}
	}
}

func TestCompactTransferProgressStatusHonorsDisplayCellsForUnicode(t *testing.T) {
	status := `[9/20] 数据\超长实验目录\另一个很长的目录\非常非常长的文件名字_参数_最终结果.emd`
	got := compactTransferProgressStatusWidth(status, 42)
	if width := pb.CellCount(got); width > 42 {
		t.Fatalf("unicode compacted status width=%d > 42: %q", width, got)
	}
	if !strings.Contains(got, ".emd") {
		t.Fatalf("unicode compacted status lost extension: %q", got)
	}
}

func TestFormatTransferProgressBytes(t *testing.T) {
	tests := map[int64]string{
		0:            "0B",
		1023:         "1023B",
		1024:         "1.0KiB",
		456824522:    "435.7MiB",
		146408155716: "136.4GiB",
	}
	for input, want := range tests {
		if got := formatTransferProgressBytes(input); got != want {
			t.Fatalf("formatTransferProgressBytes(%d)=%q want %q", input, got, want)
		}
	}
}
