package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DiskInfo holds information about a mounted disk
type DiskInfo struct {
	Device       string  `json:"device"`
	MountPoint   string  `json:"mount_point"`
	FSType       string  `json:"fs_type"`
	Total        uint64  `json:"total"`
	Used         uint64  `json:"used"`
	Available    uint64  `json:"available"`
	UsedPercent  float64 `json:"used_percent"`
	TotalHuman   string  `json:"total_human"`
	UsedHuman    string  `json:"used_human"`
	AvailHuman   string  `json:"avail_human"`
	Model        string  `json:"model"`
	SmartStatus  string  `json:"smart_status"`
	DiskID       string  `json:"disk_id"`
	BaseDevice   string  `json:"base_device"`
}

// FileEntry holds info about a large file
type FileEntry struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	SizeStr    string   `json:"size_str"`
	IsDir      bool     `json:"is_dir"`
	ModTime    string   `json:"mod_time"`
	NLinks     uint64   `json:"nlinks"`
	Inode      uint64   `json:"inode"`
	OtherPaths []string `json:"other_paths,omitempty"`
}

type DeleteRequest struct {
	Paths []string `json:"paths"`
}

type DeleteResult struct {
	Path    string `json:"path"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// MountPoint holds /proc/mounts entry for path resolution
type MountPoint struct {
	Device string
	Mount  string
}

// SmartAttr holds one parsed SMART attribute with interpretation
type SmartAttr struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Value  int    `json:"value"`
	Worst  int    `json:"worst"`
	Thresh int    `json:"thresh"`
	Raw    string `json:"raw"`
	Flag   string `json:"flag"`
	Status string `json:"status"`
	Info   string `json:"info"`
}

// SmartInfo holds the full SMART data for a disk
type SmartInfo struct {
	Device     string     `json:"device"`
	Model      string     `json:"model"`
	Serial     string     `json:"serial"`
	Firmware   string     `json:"firmware"`
	Health     string     `json:"health"`
	Attributes []SmartAttr `json:"attributes"`
}

//go:embed index.html
var indexHTML embed.FS
var hostRoot string

func main() {
	hostRoot = os.Getenv("HOST_ROOT")

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/disks", handleDisks)
	mux.HandleFunc("/api/largest-files", handleLargestFiles)
	mux.HandleFunc("/api/smart", handleSmart)
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/", handleIndex)

	log.Printf("Server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl, err := template.ParseFS(indexHTML, "index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("template error: %v", err)
	}
}

func handleDisks(w http.ResponseWriter, r *http.Request) {
	disks, err := getDisks()
	if err != nil {
		jsonError(w, err.Error())
		return
	}
	jsonResponse(w, disks)
}

func handleLargestFiles(w http.ResponseWriter, r *http.Request) {
	dirs := r.URL.Query()["dir"]
	if len(dirs) == 0 {
		dirs = []string{"/home"}
	}
	limit := 50

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var all []FileEntry
	for _, dir := range dirs {
		files, err := findLargeFiles(ctx, dir, limit)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		all = append(all, files...)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Size > all[j].Size
	})
	if len(all) > limit {
		all = all[:limit]
	}

	// Deduplicate by inode: keep one entry per inode, merge other_paths
	mounts := getMountTable()
	inodeCache := make(map[uint64][]string)
	seenInode := make(map[uint64]bool)
	deduped := make([]FileEntry, 0, len(all))
	for _, f := range all {
		if f.Inode > 0 && f.NLinks > 1 {
			if seenInode[f.Inode] {
				continue
			}
			seenInode[f.Inode] = true
			mount := findMountForPath(f.Path, mounts)
			if mount != "" {
				if cached, ok := inodeCache[f.Inode]; ok {
					f.OtherPaths = cached
				} else {
					paths := findOtherPaths(mount, f.Inode, f.Path)
					if len(paths) > 0 {
						f.OtherPaths = paths
						inodeCache[f.Inode] = paths
					}
				}
			}
		}
		deduped = append(deduped, f)
	}

	jsonResponse(w, deduped)
}

func handleSmart(w http.ResponseWriter, r *http.Request) {
	dev := r.URL.Query().Get("dev")
	if dev == "" {
		jsonError(w, "missing dev parameter")
		return
	}

	validDev := regexp.MustCompile(`^(sd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)
	if !validDev.MatchString(dev) {
		jsonError(w, "invalid device name")
		return
	}

	// smartctl -a output
	cmd := exec.Command("smartctl", "-a", "/dev/"+dev)
	out, err := cmd.Output()
	if err != nil {
		jsonError(w, fmt.Sprintf("smartctl failed for /dev/%s: %v", dev, err))
		return
	}

	info := SmartInfo{
		Device:     "/dev/" + dev,
		Attributes: []SmartAttr{},
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	inAttr := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "SMART overall-health"):
			parts := strings.Fields(line)
			info.Health = parts[len(parts)-1]
		case strings.HasPrefix(line, "Device Model:"):
			info.Model = strings.TrimSpace(strings.TrimPrefix(line, "Device Model:"))
		case strings.HasPrefix(line, "Serial Number:"):
			info.Serial = strings.TrimSpace(strings.TrimPrefix(line, "Serial Number:"))
		case strings.HasPrefix(line, "Firmware Version:"):
			info.Firmware = strings.TrimSpace(strings.TrimPrefix(line, "Firmware Version:"))
		case strings.HasPrefix(line, "ID#") && strings.Contains(line, "ATTRIBUTE_NAME"):
			inAttr = true
			continue
		case inAttr:
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "SMART") || strings.HasPrefix(line, "=== ") {
				inAttr = false
				continue
			}
			attr := parseSmartAttr(line)
			if attr != nil {
				info.Attributes = append(info.Attributes, *attr)
			}
		}
	}

	jsonResponse(w, info)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req DeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body")
		return
	}

	if len(req.Paths) == 0 {
		jsonError(w, "no paths provided")
		return
	}

	mounts := getMountTable()

	results := make([]DeleteResult, 0, len(req.Paths))
	for _, path := range req.Paths {
		cleanPath := filepath.Clean(path)
		if !filepath.IsAbs(cleanPath) {
			results = append(results, DeleteResult{Path: path, Success: false, Error: "path must be absolute"})
			continue
		}
		if !isAllowedDeletePath(cleanPath, mounts) {
			results = append(results, DeleteResult{Path: path, Success: false, Error: "path not within allowed mount points"})
			continue
		}
		fsPath := filepath.Join(hostRoot, cleanPath)
		err := os.Remove(fsPath)
		result := DeleteResult{Path: path, Success: err == nil}
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}

	jsonResponse(w, results)
}

func isAllowedDeletePath(path string, mounts []MountPoint) bool {
	for _, m := range mounts {
		if m.Mount == "/" || m.Mount == "/boot" || m.Mount == "/boot/efi" {
			continue
		}
		if strings.HasPrefix(path, m.Mount) {
			if path == m.Mount {
				return false
			}
			suffix := strings.TrimPrefix(path, m.Mount)
			if strings.HasPrefix(suffix, "/") {
				return true
			}
		}
	}
	return false
}

func parseSmartAttr(line string) *SmartAttr {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return nil
	}
	// Parse from the right: RAW_VALUE, WHEN_FAILED, UPDATED, TYPE, THRESH, WORST, VALUE, FLAG, then ID + NAME
	raw := fields[len(fields)-1]
	flag := fields[len(fields)-2]    // WHEN_FAILED (or -)
	_ = fields[len(fields)-3]        // UPDATED (Always/Offline)
	_ = fields[len(fields)-4]        // TYPE (Old_age/Pre-fail)
	thresh := atoi(fields[len(fields)-5])
	worst := atoi(fields[len(fields)-6])
	value := atoi(fields[len(fields)-7])
	_ = fields[len(fields)-8]        // FLAG (hex)
	id := atoi(fields[0])
	name := strings.Join(fields[1:len(fields)-8], " ")

	return &SmartAttr{
		ID:     id,
		Name:   name,
		Value:  value,
		Worst:  worst,
		Thresh: thresh,
		Raw:    raw,
		Flag:   flag,
		Status: interpretSmartFlag(flag),
		Info:   interpretSmartAttr(name, id, value, raw),
	}
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func interpretSmartFlag(flag string) string {
	switch flag {
	case "FAILING_NOW":
		return "critical"
	case "In_the_past":
		return "warning"
	case "-":
		return "ok"
	default:
		if strings.Contains(flag, "FAIL") {
			return "critical"
		}
		return "ok"
	}
}

func interpretSmartAttr(name string, id int, value int, raw string) string {
	switch id {
	case 1:
		return "Rate of read errors. Lower is better. A high value may indicate a failing disk."
	case 3:
		return "Time to spin up to speed. Higher values can indicate bearing/mechanical issues."
	case 4:
		return "Number of times the spindle motor has been started. High values = older drive."
	case 5:
		return "Number of bad sectors that have been remapped. Should be ZERO. Any increase = replace disk."
	case 7:
		return "Rate of seek errors. Higher values = positioning mechanism degradation."
	case 9:
		return "Total hours the disk has been powered on. ~8760h = 1 year of 24/7 use."
	case 10:
		return "Number of retries to spin up. Should be 0. Non-zero = power/mechanical issues."
	case 12:
		return "Number of power on/off cycles. Higher is normal for desktop use."
	case 173:
		return "SSD wear indicator. Compares max vs avg erase count. Lower is better."
	case 174:
		return "Unexpected power loss count. High values risk data corruption."
	case 175:
		return "NAND program failures. Should be 0. Non-zero = failing NAND."
	case 176:
		return "NAND erase failures. Should be 0. Non-zero = failing NAND."
	case 177:
		return "SSD wear leveling count. Higher = more wear on NAND cells."
	case 180:
		return "Number of unused reserved blocks. Decreasing = wear is consuming reserve."
	case 183:
		return "SATA speed negotiation downshifts. Should be 0. Non-zero = cable/controller issue."
	case 184:
		return "I/O CRC errors between disk and controller. Should be 0. Non-zero = cable issue."
	case 187:
		return "Errors that were reported to the OS but uncorrectable by ECC. Should be 0."
	case 188:
		return "Command timeouts. Should be 0. High values = disk struggling to complete commands."
	case 189:
		return "High Fly Writes — head flying too close to platter. Should be 0."
	case 190:
		return "Drive temperature. 30-50°C normal. Above 60°C reduces lifespan."
	case 191:
		return "G-sensor events (shocks). Should be 0. High values = physical impacts."
	case 192:
		return "Emergency park/retract count (power loss or shock). Should be low."
	case 193:
		return "Head load/unload cycles. High values = frequent parking, normal in laptops."
	case 194:
		return "Current drive temperature in Celsius. 30-50°C normal operating range."
	case 196:
		return "Number of reallocation events. Should be 0. Any increase = disk failing."
	case 197:
		return "Sectors waiting to be reallocated. Should be 0. Non-zero = imminent failure."
	case 198:
		return "Sectors that could not be read or corrected offline. Should be 0."
	case 199:
		return "UltraDMA CRC errors — cable/connection issue. Should be 0. Check SATA cable."
	case 200:
		return "Error rate for writes to the media. Should be 0 or very low."
	case 201:
		return "Errors detected when reading the soft-read data. Should be 0."
	case 220:
		return "Disk internal health indicator. Lower values = closer to failure."
	case 231:
		return "SSD life remaining (percentage). 100 = new, 0 = end of life."
	case 232:
		return "Available reserved SSD space as percentage. Decreasing = drive wearing out."
	case 233:
		return "Media wearout indicator (0-100). 100 = new, decreasing = wear."
	case 234:
		return "Average erase count of NAND blocks. Higher = more wear."
	case 235:
		return "Good NAND blocks remaining. Decreasing = NAND degradation."
	case 240:
		return "Total LBAs written / hours operating. Higher = more data written."
	case 241:
		return "Total data written to the disk over its lifetime (in LBAs)."
	case 242:
		return "Total data read from the disk over its lifetime (in LBAs)."
	case 250:
		return "Read error retry rate. Higher = disk struggling to read data."
	default:
		return ""
	}
}

func getDisks() ([]DiskInfo, error) {
	mountsPath := "/proc/mounts"
	if hostRoot != "" {
		mountsPath = "/proc/1/mounts"
	}
	file, err := os.Open(mountsPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var disks []DiskInfo
	seen := make(map[string]bool)

	var device, mountPoint, fsType, opts string
	var freq, pass int
	for {
		_, err := fmt.Fscanf(file, "%s %s %s %s %d %d\n", &device, &mountPoint, &fsType, &opts, &freq, &pass)
		if err != nil {
			break
		}
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		if strings.HasPrefix(device, "/dev/loop") {
			continue
		}
		if strings.HasPrefix(fsType, "fuse") || fsType == "tmpfs" || fsType == "devpts" || fsType == "proc" || fsType == "sysfs" || fsType == "cgroup" || fsType == "cgroup2" || fsType == "devtmpfs" || fsType == "pstore" || fsType == "securityfs" || fsType == "efivarfs" || fsType == "autofs" || fsType == "mqueue" || fsType == "debugfs" || fsType == "tracefs" || fsType == "hugetlbfs" || fsType == "configfs" || fsType == "bpf" || fsType == "none" {
			continue
		}
		if seen[mountPoint] {
			continue
		}
		seen[mountPoint] = true

		fsPath := filepath.Join(hostRoot, mountPoint)
		var stat syscall.Statfs_t
		if err := syscall.Statfs(fsPath, &stat); err != nil {
			continue
		}

		total := stat.Blocks * uint64(stat.Bsize)
		avail := stat.Bavail * uint64(stat.Bsize)
		used := total - avail
		var pct float64
		if total > 0 {
			pct = float64(used) / float64(total) * 100
		}

		base := getBaseDevice(device)
		model := ""
		smart := ""
		diskID := ""
		if base != "" {
			model = getDiskModel(base)
			smart = getSmartStatus(base)
			diskID = getDiskID(base)
		}

		disks = append(disks, DiskInfo{
			Device:       device,
			MountPoint:   mountPoint,
			FSType:       fsType,
			Total:        total,
			Used:         used,
			Available:    avail,
			UsedPercent:  pct,
			TotalHuman:   formatBytes(total),
			UsedHuman:    formatBytes(used),
			AvailHuman:   formatBytes(avail),
			Model:        model,
			SmartStatus:  smart,
			DiskID:       diskID,
			BaseDevice:   base,
		})
	}

	sort.Slice(disks, func(i, j int) bool {
		return disks[i].MountPoint < disks[j].MountPoint
	})

	return disks, nil
}

func getBaseDevice(device string) string {
	if strings.HasPrefix(device, "/dev/mapper/") {
		entries, err := os.ReadDir("/sys/block")
		if err != nil {
			return ""
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "dm-") {
				continue
			}
			slaves, err := os.ReadDir("/sys/block/" + name + "/slaves")
			if err != nil {
				continue
			}
			dmName, _ := os.ReadFile("/sys/block/" + name + "/dm/name")
			dmMapped := strings.TrimSpace(string(dmName))
			if strings.Contains(device, dmMapped) {
				for _, slave := range slaves {
					name := slave.Name()
					re := regexp.MustCompile(`^(sd[a-z]+|nvme\d+n\d+|mmcblk\d+)`)
					m := re.FindString(name)
					if m != "" {
						return m
					}
					return name
				}
			}
		}
		return ""
	}
	re := regexp.MustCompile(`^/dev/(sd[a-z]+|nvme\d+n\d+|mmcblk\d+)`)
	matches := re.FindStringSubmatch(device)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func getDiskModel(base string) string {
	data, err := os.ReadFile("/sys/block/" + base + "/device/model")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func getSmartStatus(base string) string {
	cmd := exec.Command("smartctl", "-H", "/dev/"+base)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "SMART overall-health") {
			parts := strings.Fields(line)
			return parts[len(parts)-1]
		}
	}
	return ""
}

func getDiskID(base string) string {
	matches, err := filepath.Glob("/dev/disk/by-id/*" + base + "*")
	if err != nil || len(matches) == 0 {
		entries, err := os.ReadDir("/dev/disk/by-id")
		if err != nil {
			return ""
		}
		for _, e := range entries {
			link, _ := os.Readlink("/dev/disk/by-id/" + e.Name())
			if strings.HasSuffix(link, "/"+base) && !strings.Contains(e.Name(), "-part") {
				return e.Name()
			}
		}
		return ""
	}
	for _, m := range matches {
		if !strings.Contains(m, "-part") {
			name := strings.TrimPrefix(m, "/dev/disk/by-id/")
			return name
		}
	}
	return ""
}

func findLargeFiles(ctx context.Context, root string, limit int) ([]FileEntry, error) {
	var files []FileEntry
	walked := 0

	walkRoot := filepath.Join(hostRoot, root)

	err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			return filepath.SkipDir
		}
		if walked > 100000 {
			return fs.SkipAll
		}
		walked++

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info.Size() < 100*1024*1024 {
			return nil
		}

		nlinks := uint64(1)
		inode := uint64(0)
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			nlinks = uint64(stat.Nlink)
			inode = stat.Ino
		}

		displayPath := path
		if hostRoot != "" {
			displayPath = strings.TrimPrefix(path, hostRoot)
		}
		files = append(files, FileEntry{
			Name:    d.Name(),
			Path:    displayPath,
			Size:    info.Size(),
			SizeStr: formatBytes(uint64(info.Size())),
			IsDir:   d.IsDir(),
			ModTime: info.ModTime().Format(time.RFC3339),
			NLinks:  nlinks,
			Inode:   inode,
		})
		return nil
	})

	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Size > files[j].Size
	})
	if len(files) > limit {
		files = files[:limit]
	}

	return files, nil
}

func getMountTable() []MountPoint {
	mountsPath := "/proc/mounts"
	if hostRoot != "" {
		mountsPath = "/proc/1/mounts"
	}
	file, err := os.Open(mountsPath)
	if err != nil {
		return nil
	}
	defer file.Close()

	var mounts []MountPoint
	var device, mountPoint, fsType, opts string
	var freq, pass int
	for {
		_, err := fmt.Fscanf(file, "%s %s %s %s %d %d\n", &device, &mountPoint, &fsType, &opts, &freq, &pass)
		if err != nil {
			break
		}
		if strings.HasPrefix(device, "/dev/") && !strings.HasPrefix(device, "/dev/loop") {
			mounts = append(mounts, MountPoint{Device: device, Mount: mountPoint})
		}
	}
	return mounts
}

func findMountForPath(path string, mounts []MountPoint) string {
	var best string
	for _, m := range mounts {
		if strings.HasPrefix(path, m.Mount) && len(m.Mount) > len(best) {
			best = m.Mount
		}
	}
	return best
}

func findOtherPaths(mount string, inode uint64, excludePath string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fsMount := mount
	if hostRoot != "" {
		fsMount = filepath.Join(hostRoot, mount)
	}

	cmd := exec.CommandContext(ctx, "find", fsMount, "-xdev", "-inum", fmt.Sprintf("%d", inode))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var paths []string
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p = strings.TrimSpace(p)
		if hostRoot != "" {
			p = strings.TrimPrefix(p, hostRoot)
		}
		if p != "" && p != excludePath {
			paths = append(paths, p)
		}
	}
	return paths
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
