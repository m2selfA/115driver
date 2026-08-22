package output

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cheggaaa/pb/v3"
)

type TransferProgress struct {
	mu      sync.Mutex
	enabled bool
	bar     *pb.ProgressBar
	status  string
}

const transferProgressTemplate = `{{transferstatus .}}  {{string . "bytes"}}  {{percent . }}  {{speed . "%s/s" "?/s"}}`

var transferProgressElementOnce sync.Once

func ensureTransferProgressElement() {
	transferProgressElementOnce.Do(func() {
		pb.RegisterElement("transferstatus", pb.ElementFunc(func(state *pb.State, _ ...string) string {
			status, _ := state.Get("status").(string)
			return compactTransferProgressStatusWidth(status, state.AdaptiveElWidth())
		}), true)
	})
}

func NewTransferProgress() *TransferProgress {
	return &TransferProgress{enabled: terminalFile(os.Stderr)}
}

func (progress *TransferProgress) Enabled() bool {
	return progress != nil && progress.enabled
}

func (progress *TransferProgress) SetStatus(status string) {
	if progress == nil {
		return
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	progress.status = normalizeTransferProgressStatus(status)
	if progress.bar != nil {
		progress.bar.Set("status", progress.status)
	}
}

func (progress *TransferProgress) SetProgress(completed, total int64) {
	if progress == nil || !progress.enabled || total < 0 {
		return
	}
	if completed < 0 {
		completed = 0
	}
	if completed > total {
		completed = total
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.bar == nil {
		progress.bar = pb.New64(total)
		progress.bar.SetWriter(os.Stderr)
		configureTransferProgressBar(progress.bar, progress.status, completed, total)
		progress.bar.Start()
	} else {
		progress.bar.SetTotal(total)
		progress.bar.Set("bytes", formatTransferProgressCounters(completed, total))
	}
	progress.bar.SetCurrent(completed)
}

func (progress *TransferProgress) Finish() {
	if progress == nil {
		return
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.bar != nil {
		progress.bar.Finish()
		progress.bar = nil
	}
}

func terminalFile(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func configureTransferProgressBar(bar *pb.ProgressBar, status string, completed, total int64) {
	ensureTransferProgressElement()
	bar.Set(pb.Bytes, true)
	bar.SetTemplateString(transferProgressTemplate)
	bar.Set("status", normalizeTransferProgressStatus(status))
	bar.Set("bytes", formatTransferProgressCounters(completed, total))
}

func formatTransferProgressCounters(completed, total int64) string {
	return formatTransferProgressBytes(completed) + "/" + formatTransferProgressBytes(total)
}

func formatTransferProgressBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	if value < 1024 {
		return fmt.Sprintf("%dB", value)
	}
	scaled := float64(value)
	unit := 0
	for scaled >= 1024 && unit < len(units)-1 {
		scaled /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f%s", scaled, units[unit])
}

func normalizeTransferProgressStatus(status string) string {
	return strings.Join(strings.Fields(status), " ")
}

func compactTransferProgressStatus(status string) string {
	return compactTransferProgressStatusWidth(status, 72)
}

func compactTransferProgressStatusWidth(status string, maxCells int) string {
	status = normalizeTransferProgressStatus(status)
	if maxCells <= 0 || status == "" {
		return ""
	}
	if pb.CellCount(status) <= maxCells {
		return status
	}

	prefix, subject, detail, indexed := splitIndexedTransferProgressStatus(status)
	if !indexed {
		return compactTransferTextMiddle(status, maxCells)
	}
	prefixCells := pb.CellCount(prefix)
	if prefixCells >= maxCells {
		return compactTransferTextPrefix(prefix, maxCells)
	}
	budget := maxCells - prefixCells
	if detail == "" {
		return prefix + compactTransferPath(subject, budget)
	}

	const detailMarker = " — "
	markerCells := pb.CellCount(detailMarker)
	if budget <= markerCells+12 {
		return prefix + compactTransferPath(subject, budget)
	}
	detailBudget := budget / 3
	if detailBudget > 28 {
		detailBudget = 28
	}
	if detailBudget < 10 {
		detailBudget = 10
	}
	if retryAt := strings.Index(detail, "retry "); retryAt >= 0 {
		minimumRetryBudget := pb.CellCount(detail[:retryAt+len("retry")]) + 1
		if minimumRetryBudget > detailBudget && minimumRetryBudget <= 28 {
			detailBudget = minimumRetryBudget
		}
	}
	pathBudget := budget - markerCells - detailBudget
	if pathBudget < 16 {
		detailBudget = budget - markerCells - 16
		pathBudget = 16
	}
	if detailBudget < 6 {
		return prefix + compactTransferPath(subject, budget)
	}
	return prefix + compactTransferPath(subject, pathBudget) + detailMarker + compactTransferTextPrefix(detail, detailBudget)
}

func splitIndexedTransferProgressStatus(status string) (prefix, subject, detail string, ok bool) {
	if !strings.HasPrefix(status, "[") {
		return "", "", "", false
	}
	end := strings.Index(status, "] ")
	if end < 0 {
		return "", "", "", false
	}
	prefix = status[:end+2]
	rest := status[end+2:]
	if rest == "" {
		return "", "", "", false
	}
	if detailAt := strings.Index(rest, " — "); detailAt >= 0 {
		subject = rest[:detailAt]
		detail = rest[detailAt+len(" — "):]
	} else {
		subject = rest
	}
	return prefix, subject, detail, true
}

func compactTransferPath(value string, maxCells int) string {
	value = strings.TrimSpace(value)
	if maxCells <= 0 || value == "" {
		return ""
	}
	if pb.CellCount(value) <= maxCells {
		return value
	}
	separatorAt := strings.LastIndexAny(value, `/\\`)
	if separatorAt < 0 {
		return compactTransferFileName(value, maxCells)
	}
	directory, fileName := value[:separatorAt], value[separatorAt+1:]
	separator := value[separatorAt : separatorAt+1]
	if directory == "" {
		return separator + compactTransferFileName(fileName, maxCells-pb.CellCount(separator))
	}
	if fileName == "" {
		return compactTransferDirectory(directory, maxCells, separator)
	}

	separatorCells := pb.CellCount(separator)
	directoryCells := pb.CellCount(directory)
	fileCells := pb.CellCount(fileName)
	if maxCells <= separatorCells+2 {
		return compactTransferTextMiddle(value, maxCells)
	}

	directoryBudget := maxCells / 3
	if directoryBudget < 6 {
		directoryBudget = 6
	}
	if directoryBudget > directoryCells {
		directoryBudget = directoryCells
	}
	fileBudget := maxCells - separatorCells - directoryBudget
	if fileCells < fileBudget {
		fileBudget = fileCells
		directoryBudget = maxCells - separatorCells - fileBudget
	}
	if directoryCells < directoryBudget {
		directoryBudget = directoryCells
		fileBudget = maxCells - separatorCells - directoryBudget
	}
	if fileBudget <= 0 || directoryBudget <= 0 {
		return compactTransferTextMiddle(value, maxCells)
	}
	return compactTransferDirectory(directory, directoryBudget, separator) + separator + compactTransferFileName(fileName, fileBudget)
}

func compactTransferDirectory(directory string, maxCells int, separator string) string {
	if maxCells <= 0 {
		return ""
	}
	if pb.CellCount(directory) <= maxCells {
		return directory
	}
	segments := strings.FieldsFunc(directory, func(r rune) bool { return r == '/' || r == '\\' })
	if len(segments) >= 3 {
		candidate := segments[0] + separator + "…" + separator + strings.Join(segments[len(segments)-2:], separator)
		if pb.CellCount(candidate) <= maxCells {
			return candidate
		}
	}
	if len(segments) >= 2 {
		candidate := segments[0] + separator + "…" + separator + segments[len(segments)-1]
		if pb.CellCount(candidate) <= maxCells {
			return candidate
		}
	}
	return compactTransferTextMiddle(directory, maxCells)
}

func compactTransferFileName(fileName string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	if pb.CellCount(fileName) <= maxCells {
		return fileName
	}
	dot := strings.LastIndex(fileName, ".")
	if dot > 0 && dot < len(fileName)-1 {
		extension := fileName[dot:]
		extensionCells := pb.CellCount(extension)
		baseBudget := maxCells - extensionCells
		if baseBudget >= 3 {
			return compactTransferTextMiddle(fileName[:dot], baseBudget) + extension
		}
	}
	return compactTransferTextMiddle(fileName, maxCells)
}

func compactTransferTextPrefix(value string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	if pb.CellCount(value) <= maxCells {
		return value
	}
	if maxCells == 1 {
		return "…"
	}
	return takeTransferLeftCells(value, maxCells-1) + "…"
}

func compactTransferTextMiddle(value string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	if pb.CellCount(value) <= maxCells {
		return value
	}
	if maxCells == 1 {
		return "…"
	}
	contentCells := maxCells - 1
	leftCells := (contentCells + 1) / 2
	rightCells := contentCells - leftCells
	return takeTransferLeftCells(value, leftCells) + "…" + takeTransferRightCells(value, rightCells)
}

func takeTransferLeftCells(value string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	used := 0
	end := 0
	for index, r := range value {
		width := pb.CellCount(string(r))
		if width > 0 && used+width > maxCells {
			break
		}
		used += width
		end = index + len(string(r))
	}
	return value[:end]
}

func takeTransferRightCells(value string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	runes := []rune(value)
	used := 0
	start := len(runes)
	for index := len(runes) - 1; index >= 0; index-- {
		width := pb.CellCount(string(runes[index]))
		if width > 0 && used+width > maxCells {
			break
		}
		used += width
		start = index
	}
	return string(runes[start:])
}
