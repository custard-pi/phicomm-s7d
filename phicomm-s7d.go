// phicomm-s7d is a self-contained Phicomm S7 protocol emulator, recorder and web UI.
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

const version = "1.0"

const defaultConfigPath = "/etc/phicom-s7/config.json"

//go:embed favicon.svg
var faviconSVG []byte

var (
	decodedHeader = []byte{0x55, 0xaa}
	wireHeader    = []byte{0x00, 0xff}
)

type config struct {
	tcpListen  string
	httpListen string
	dataFile   string
	heightCM   float64
	coefSet    string
	strictCRC  bool
	configPath string
}

type diskConfig struct {
	TCPListen  string  `json:"tcp_listen"`
	HTTPListen string  `json:"http_listen"`
	DataFile   string  `json:"data_file"`
	HeightCM   float64 `json:"height_cm"`
	CoefSet    string  `json:"coef_set"`
	StrictCRC  bool    `json:"strict_crc"`
}

type measurement struct {
	ID                 string    `json:"id"`
	ReceivedAt         string    `json:"received_at"`
	DeviceMAC          string    `json:"device_mac"`
	WeightKG           float64   `json:"weight_kg"`
	ImpedanceOhm       []float64 `json:"impedance_ohm"`
	Status             int       `json:"status"`
	FatPercent         *float64  `json:"fat_percent"`
	FatChannel         int       `json:"fat_impedance_channel"`
	PlausibleChannels  []int     `json:"plausible_channels"`
	CRCValid           bool      `json:"crc_valid"`
	DecodedHex         string    `json:"decoded_hex,omitempty"`
	LegacyFirstChannel *float64  `json:"fat_percent_first_channel,omitempty"`
}

func defaultDiskConfig() diskConfig {
	return diskConfig{
		TCPListen: ":30101", HTTPListen: ":8088",
		DataFile: "/etc/phicom-s7/measurements.jsonl",
		HeightCM: 180, CoefSet: "male", StrictCRC: true,
	}
}

func validateDiskConfig(value diskConfig, path string) (config, error) {
	if value.TCPListen == "" || value.HTTPListen == "" || value.DataFile == "" {
		return config{}, errors.New("listen addresses and data_file must not be empty")
	}
	if value.HeightCM < 50 || value.HeightCM > 250 {
		return config{}, errors.New("height_cm must be between 50 and 250")
	}
	if value.CoefSet != "male" && value.CoefSet != "female" {
		return config{}, errors.New("coef_set must be male or female")
	}
	return config{tcpListen: value.TCPListen, httpListen: value.HTTPListen, dataFile: value.DataFile, heightCM: value.HeightCM, coefSet: value.CoefSet, strictCRC: value.StrictCRC, configPath: path}, nil
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var value diskConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return config{}, err
	}
	return validateDiskConfig(value, path)
}

func prompt(reader *bufio.Reader, label, fallback string) (string, error) {
	fmt.Printf("%s [%s]: ", label, fallback)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return fallback, nil
	}
	return line, nil
}

func interactiveConfig(path string) (config, error) {
	defaults := defaultDiskConfig()
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Configure phicomm-s7d; press Enter to accept each default.\nConfig file: %s\n", path)
	var err error
	if defaults.TCPListen, err = prompt(reader, "S7 TCP listen", defaults.TCPListen); err != nil {
		return config{}, err
	}
	if defaults.HTTPListen, err = prompt(reader, "Web HTTP listen", defaults.HTTPListen); err != nil {
		return config{}, err
	}
	if defaults.DataFile, err = prompt(reader, "Measurement JSONL", defaults.DataFile); err != nil {
		return config{}, err
	}
	height, err := prompt(reader, "Height in cm", strconv.FormatFloat(defaults.HeightCM, 'f', -1, 64))
	if err != nil {
		return config{}, err
	}
	defaults.HeightCM, err = strconv.ParseFloat(height, 64)
	if err != nil {
		return config{}, fmt.Errorf("invalid height: %w", err)
	}
	if defaults.CoefSet, err = prompt(reader, "Regression coefficient set (male/female)", defaults.CoefSet); err != nil {
		return config{}, err
	}
	validated, err := validateDiskConfig(defaults, path)
	if err != nil {
		return config{}, err
	}
	data, err := json.MarshalIndent(defaults, "", "  ")
	if err != nil {
		return config{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return config{}, err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return config{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return config{}, err
	}
	fmt.Printf("Saved configuration to %s\n", path)
	return validated, nil
}

type store struct {
	mu      sync.RWMutex
	path    string
	profile config
	items   []measurement
	total   int
}

const recentCacheLimit = 5000

type pageResult struct {
	Items    []measurement `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

func xor55(data []byte) []byte {
	out := make([]byte, len(data))
	for i, value := range data {
		out[i] = value ^ 0x55
	}
	return out
}

func crc16Modbus(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, value := range data {
		crc ^= uint16(value)
		for range 8 {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func validFrameCRC(frame []byte) bool {
	return len(frame) >= 8 && bytes.Equal(frame[:2], decodedHeader) &&
		int(binary.BigEndian.Uint16(frame[2:4])) == len(frame) &&
		crc16Modbus(frame[2:len(frame)-4]) == binary.BigEndian.Uint16(frame[len(frame)-2:])
}

func protocolTime(now time.Time) []byte {
	return []byte{byte(now.Year() % 100), byte(now.Month()), byte(now.Day()), byte(now.Hour()), byte(now.Minute()), byte(now.Second())}
}

func finishFrame(prefix []byte, total int) ([]byte, error) {
	if len(prefix) > total-4 {
		return nil, errors.New("response prefix too long")
	}
	decoded := make([]byte, total)
	copy(decoded, prefix)
	crc := crc16Modbus(decoded[2 : total-4])
	binary.BigEndian.PutUint16(decoded[total-2:], crc)
	return xor55(decoded), nil
}

func responseFor(request []byte, now time.Time) ([]byte, error) {
	if len(request) < 28 || !bytes.Equal(request[:2], decodedHeader) {
		return nil, errors.New("invalid request")
	}
	command := binary.BigEndian.Uint16(request[26:28])
	var responseCommand uint16
	var total int
	switch command {
	case 0x8000:
		responseCommand, total = 0xc000, 48
	case 0x8062:
		responseCommand, total = 0xc062, 64
	case 0x8052:
		responseCommand, total = 0xc052, 32
	case 0x8090:
		responseCommand, total = 0xc090, 64
	default:
		return nil, fmt.Errorf("unsupported command 0x%04x", command)
	}

	prefix := make([]byte, 0, total)
	prefix = append(prefix, decodedHeader...)
	prefix = binary.BigEndian.AppendUint16(prefix, uint16(total))
	prefix = append(prefix, request[4:12]...)
	prefix = append(prefix, request[18:24]...)
	prefix = append(prefix, request[12:18]...)
	prefix = append(prefix, request[24:26]...)
	prefix = binary.BigEndian.AppendUint16(prefix, responseCommand)
	if command == 0x8000 {
		prefix = append(prefix, protocolTime(now)...)
		prefix = append(prefix, 0x02, 0, 0, 0, 0x01, 0x55)
	} else if command == 0x8062 || command == 0x8090 {
		prefix = append(prefix, protocolTime(now)...)
		prefix = append(prefix, 0x02)
	}
	return finishFrame(prefix, total)
}

func bodyFat(weight, height, impedance float64, coefSet string) float64 {
	h2r := height * height / impedance
	var ffm float64
	if coefSet == "male" {
		ffm = -10.68 + 0.65*h2r + 0.26*weight + 0.02*impedance
	} else {
		ffm = -9.53 + 0.69*h2r + 0.17*weight + 0.02*impedance
	}
	return (1 - ffm/weight) * 100
}

func rounded(value float64) float64 {
	return float64(int(value*100+0.5)) / 100
}

func measurementID(receivedAt, decoded string) string {
	// Stable FNV-1a identifier; sufficient for identifying a local JSONL row.
	value := uint64(14695981039346656037)
	for _, char := range receivedAt + "\x00" + decoded {
		value ^= uint64(char)
		value *= 1099511628211
	}
	return fmt.Sprintf("%016x", value)
}

func parseMeasurement(frame []byte, cfg config) (*measurement, error) {
	if len(frame) != 64 || !bytes.Equal(frame[26:28], []byte{0x80, 0x62}) {
		return nil, errors.New("not a measurement frame")
	}
	weight := float64(binary.BigEndian.Uint16(frame[43:45])) / 100
	impedance := make([]float64, 6)
	plausible := make([]int, 0, 6)
	for i := range 6 {
		impedance[i] = float64(binary.LittleEndian.Uint16(frame[45+i*2:47+i*2])) / 10
		if impedance[i] >= 250 && impedance[i] <= 800 {
			plausible = append(plausible, i+1)
		}
	}
	var fat *float64
	if weight > 0 && impedance[5] >= 250 && impedance[5] <= 800 {
		value := rounded(bodyFat(weight, cfg.heightCM, impedance[5], cfg.coefSet))
		fat = &value
	}
	item := &measurement{
		ReceivedAt:        time.Now().Format(time.RFC3339),
		DeviceMAC:         net.HardwareAddr(frame[12:18]).String(),
		WeightKG:          weight,
		ImpedanceOhm:      impedance,
		Status:            int(frame[57]),
		FatPercent:        fat,
		FatChannel:        6,
		PlausibleChannels: plausible,
		CRCValid:          validFrameCRC(frame),
		DecodedHex:        hex.EncodeToString(frame),
	}
	item.ID = measurementID(item.ReceivedAt, item.DecodedHex)
	return item, nil
}

func newStore(path string, cfg config) (*store, error) {
	s := &store{path: path, profile: cfg}
	err := s.scanUnlocked(func(item measurement) error {
		s.total++
		s.cache(item)
		return nil
	})
	return s, err
}

func (s *store) normalize(item measurement) measurement {
	if item.ID == "" {
		item.ID = measurementID(item.ReceivedAt, item.DecodedHex)
	}
	item.FatChannel = 6
	if len(item.ImpedanceOhm) >= 6 && item.WeightKG > 0 && item.ImpedanceOhm[5] >= 250 && item.ImpedanceOhm[5] <= 800 {
		value := rounded(bodyFat(item.WeightKG, s.profile.heightCM, item.ImpedanceOhm[5], s.profile.coefSet))
		item.FatPercent = &value
	}
	return item
}

func (s *store) cache(item measurement) {
	s.items = append(s.items, item)
	if len(s.items) > recentCacheLimit {
		copy(s.items, s.items[len(s.items)-recentCacheLimit:])
		s.items = s.items[:recentCacheLimit]
	}
}

func (s *store) scanUnlocked(visit func(measurement) error) error {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var item measurement
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			log.Printf("skip malformed JSONL record: %v", err)
			continue
		}
		if err := visit(s.normalize(item)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func inRange(item measurement, from, to *time.Time) bool {
	when, err := time.Parse(time.RFC3339, item.ReceivedAt)
	if err != nil {
		return false
	}
	return (from == nil || !when.Before(*from)) && (to == nil || !when.After(*to))
}

func (s *store) add(item measurement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil && filepath.Dir(s.path) != "." {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(item)
	if err == nil {
		_, err = file.Write(append(encoded, '\n'))
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	s.items = append(s.items, item)
	if len(s.items) > recentCacheLimit {
		s.items = append([]measurement(nil), s.items[len(s.items)-recentCacheLimit:]...)
	}
	s.total++
	return nil
}

func (s *store) recent(limit int) []measurement {
	s.mu.RLock()
	defer s.mu.RUnlock()
	start := 0
	if limit > 0 && len(s.items) > limit {
		start = len(s.items) - limit
	}
	out := make([]measurement, len(s.items)-start)
	copy(out, s.items[start:])
	return out
}

func (s *store) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.total
}

func (s *store) page(from, to *time.Time, page, pageSize int) (pageResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	if err := s.scanUnlocked(func(item measurement) error {
		if inRange(item, from, to) {
			total++
		}
		return nil
	}); err != nil {
		return pageResult{}, err
	}
	start := total - page*pageSize
	if start < 0 {
		start = 0
	}
	end := total - (page-1)*pageSize
	if end < 0 {
		end = 0
	}
	items := make([]measurement, 0, pageSize)
	index := 0
	if err := s.scanUnlocked(func(item measurement) error {
		if !inRange(item, from, to) {
			return nil
		}
		if index >= start && index < end {
			items = append(items, item)
		}
		index++
		return nil
	}); err != nil {
		return pageResult{}, err
	}
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	return pageResult{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

func (s *store) chart(from, to time.Time) ([]measurement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]measurement, 0)
	err := s.scanUnlocked(func(item measurement) error {
		if inRange(item, &from, &to) {
			items = append(items, item)
		}
		return nil
	})
	return items, err
}

func (s *store) delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil && filepath.Dir(s.path) != "." {
		return false, err
	}
	temporary := s.path + ".tmp"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	encoder := json.NewEncoder(output)
	found := false
	recent := make([]measurement, 0, recentCacheLimit)
	total := 0
	err = s.scanUnlocked(func(item measurement) error {
		if item.ID == id {
			found = true
			return nil
		}
		if err := encoder.Encode(item); err != nil {
			return err
		}
		total++
		recent = append(recent, item)
		if len(recent) > recentCacheLimit {
			recent = recent[1:]
		}
		return nil
	})
	if err == nil {
		err = output.Sync()
	}
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if !found {
		os.Remove(temporary)
		return false, nil
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return false, err
	}
	s.items, s.total = recent, total
	return true, nil
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	matched := 0
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if value == wireHeader[matched] {
			matched++
			if matched == len(wireHeader) {
				break
			}
		} else if value == wireHeader[0] {
			matched = 1
		} else {
			matched = 0
		}
	}
	wire := []byte{wireHeader[0], wireHeader[1], 0, 0}
	if _, err := io.ReadFull(reader, wire[2:4]); err != nil {
		return nil, err
	}
	header := xor55(wire)
	length := int(binary.BigEndian.Uint16(header[2:4]))
	if length < 32 || length > 4096 {
		return nil, fmt.Errorf("invalid frame length %d", length)
	}
	wire = append(wire, make([]byte, length-4)...)
	if _, err := io.ReadFull(reader, wire[4:]); err != nil {
		return nil, err
	}
	return xor55(wire), nil
}

func serveProtocol(listener net.Listener, cfg config, records *store) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		go handleConnection(connection, cfg, records)
	}
}

func handleConnection(connection net.Conn, cfg config, records *store) {
	defer connection.Close()
	peer := connection.RemoteAddr().String()
	log.Printf("connection from %s", peer)
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(connection)
	for {
		frame, err := readFrame(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("connection %s ended: %v", peer, err)
			}
			return
		}
		command := binary.BigEndian.Uint16(frame[26:28])
		crcOK := validFrameCRC(frame)
		log.Printf("rx %s cmd=%04x len=%d crc=%v hex=%s", peer, command, len(frame), crcOK, hex.EncodeToString(frame))
		if cfg.strictCRC && !crcOK {
			log.Printf("drop invalid CRC from %s", peer)
			continue
		}
		if item, err := parseMeasurement(frame, cfg); err == nil {
			if err := records.add(*item); err != nil {
				log.Printf("save measurement: %v", err)
			}
			fat := "null"
			if item.FatPercent != nil {
				fat = fmt.Sprintf("%.2f", *item.FatPercent)
			}
			log.Printf("measurement mac=%s weight=%.2fkg impedance=%v fat(ch6)=%s%%", item.DeviceMAC, item.WeightKG, item.ImpedanceOhm, fat)
		}
		response, err := responseFor(frame, time.Now())
		if err != nil {
			log.Printf("%v", err)
			continue
		}
		if _, err := connection.Write(response); err != nil {
			log.Printf("write response to %s: %v", peer, err)
			return
		}
		decoded := xor55(response)
		log.Printf("tx %s cmd=%04x len=%d", peer, binary.BigEndian.Uint16(decoded[26:28]), len(decoded))
	}
}

func queryDate(value string, endOfDay bool) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, time.Local)
	if err != nil {
		return nil, err
	}
	if endOfDay {
		parsed = parsed.Add(24*time.Hour - time.Nanosecond)
	}
	return &parsed, nil
}

func requestRange(r *http.Request, chart bool) (*time.Time, *time.Time, error) {
	from, err := queryDate(r.URL.Query().Get("from"), false)
	if err != nil {
		return nil, nil, errors.New("invalid from date")
	}
	to, err := queryDate(r.URL.Query().Get("to"), true)
	if err != nil {
		return nil, nil, errors.New("invalid to date")
	}
	if chart {
		now := time.Now()
		if to == nil {
			end := now
			to = &end
		}
		if from == nil {
			start := to.AddDate(0, -3, 0)
			from = &start
		}
		if to.Before(*from) || to.Sub(*from) > 366*24*time.Hour {
			return nil, nil, errors.New("chart range must be between 0 and 366 days")
		}
	} else if from != nil && to != nil && to.Before(*from) {
		return nil, nil, errors.New("to date precedes from date")
	}
	return from, to, nil
}

func serveWeb(cfg config, records *store) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, dashboardHTML)
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconSVG)
	})
	mux.HandleFunc("GET /api/measurements", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if page < 1 {
			page = 1
		}
		if pageSize != 50 && pageSize != 100 && pageSize != 200 && pageSize != 500 {
			pageSize = 50
		}
		from, to, err := requestRange(r, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := records.page(from, to, page, pageSize)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, result)
	})
	mux.HandleFunc("GET /api/chart", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := requestRange(r, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		items, err := records.chart(*from, *to)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, items)
	})
	mux.HandleFunc("DELETE /api/measurements/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) != 16 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		deleted, err := records.delete(id)
		if err != nil {
			log.Printf("delete measurement %s: %v", id, err)
			http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("GET /api/latest", func(w http.ResponseWriter, r *http.Request) {
		items := records.recent(1)
		if len(items) == 0 {
			http.Error(w, `{"error":"no measurements"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, items[0])
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"version": version, "records": records.count(), "cached_records": len(records.recent(0)), "fat_channel": 6, "height_cm": cfg.heightCM, "coef_set": cfg.coefSet})
	})
	mux.HandleFunc("GET /api/export.csv", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := requestRange(r, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="s7-measurements.csv"`)
		writer := csv.NewWriter(w)
		_ = writer.Write([]string{"time", "mac", "weight_kg", "fat_percent", "impedance_1", "impedance_2", "impedance_3", "impedance_4", "impedance_5", "impedance_6", "status"})
		records.mu.RLock()
		defer records.mu.RUnlock()
		err = records.scanUnlocked(func(item measurement) error {
			if !inRange(item, from, to) {
				return nil
			}
			row := []string{item.ReceivedAt, item.DeviceMAC, fmt.Sprintf("%.2f", item.WeightKG), ""}
			if item.FatPercent != nil {
				row[3] = fmt.Sprintf("%.2f", *item.FatPercent)
			}
			for _, value := range item.ImpedanceOhm {
				row = append(row, fmt.Sprintf("%.1f", value))
			}
			row = append(row, strconv.Itoa(item.Status))
			return writer.Write(row)
		})
		if err != nil {
			log.Printf("CSV export: %v", err)
		}
		writer.Flush()
	})
	server := &http.Server{Addr: cfg.httpListen, Handler: securityHeaders(mux), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	return server.ListenAndServe()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON: %v", err)
	}
}

func selfTest() error {
	configWithUnknownKey := `{"tcp_listen":":30101","http_listen":":8088","data_file":"measurements.jsonl","unused":"ignored","height_cm":180,"coef_set":"male","strict_crc":true}`
	var decodedConfig diskConfig
	decoder := json.NewDecoder(strings.NewReader(configWithUnknownKey))
	if err := decoder.Decode(&decodedConfig); err != nil {
		return fmt.Errorf("unknown configuration key was not ignored: %w", err)
	}
	if _, err := validateDiskConfig(decodedConfig, "config.json"); err != nil {
		return fmt.Errorf("configuration with unknown key failed validation: %w", err)
	}
	missingCoefSet := `{"tcp_listen":":30101","http_listen":":8088","data_file":"measurements.jsonl","height_cm":180,"strict_crc":true}`
	var incompleteConfig diskConfig
	if err := json.Unmarshal([]byte(missingCoefSet), &incompleteConfig); err != nil {
		return fmt.Errorf("incomplete configuration decode failed: %w", err)
	}
	if _, err := validateDiskConfig(incompleteConfig, "config.json"); err == nil {
		return errors.New("configuration without coef_set was accepted")
	}
	requestHex := "55aa00300000000100000001b4e62d0f4439ffffffffffff000080000000000155000000000000000000000000006f51"
	wantHex := "55aa00300000000100000001ffffffffffffb4e62d0f44390000c0001a0a06173a1f0200000001550000000000009568"
	request, _ := hex.DecodeString(requestHex)
	want, _ := hex.DecodeString(wantHex)
	now := time.Date(2026, 10, 6, 23, 58, 31, 0, time.Local)
	wire, err := responseFor(request, now)
	if err != nil {
		return err
	}
	if got := xor55(wire); !bytes.Equal(got, want) {
		return fmt.Errorf("C000 mismatch\ngot  %x\nwant %x", got, want)
	}
	if !validFrameCRC(request) || !validFrameCRC(want) {
		return errors.New("known CRC validation failed")
	}
	measurementHex := "55aa00400000000100000001b4e62d0f4439ffffffffffff00008062017c000000000400330005000301001b2b0604fe04fe04fe04fe04fe125500000000e748"
	frame, _ := hex.DecodeString(measurementHex)
	item, err := parseMeasurement(frame, config{heightCM: 175, coefSet: "male"})
	if err != nil || item.WeightKG != 69.55 || item.FatPercent == nil || *item.FatPercent != 16.51 {
		return fmt.Errorf("measurement test failed: %+v %v", item, err)
	}
	temporary, err := os.CreateTemp("", "phicomm-s7-selftest-*.jsonl")
	if err != nil {
		return err
	}
	path := temporary.Name()
	temporary.Close()
	defer os.Remove(path)
	testStore := &store{path: path, profile: config{heightCM: 180, coefSet: "male"}}
	if err := testStore.add(*item); err != nil {
		return err
	}
	page, err := testStore.page(nil, nil, 1, 50)
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		return fmt.Errorf("pagination test failed: %+v %v", page, err)
	}
	points, err := testStore.chart(time.Now().Add(-24*time.Hour), time.Now().Add(24*time.Hour))
	if err != nil || len(points) != 1 {
		return fmt.Errorf("chart streaming test failed: %d %v", len(points), err)
	}
	deleted, err := testStore.delete(item.ID)
	if err != nil || !deleted || len(testStore.recent(0)) != 0 {
		return fmt.Errorf("delete test failed: deleted=%v err=%v", deleted, err)
	}
	return nil
}

func main() {
	configPath := defaultConfigPath
	var doSelfTest, doConfigure, doCheckConfig bool
	flag.StringVar(&configPath, "config", defaultConfigPath, "configuration JSON path")
	flag.BoolVar(&doSelfTest, "self-test", false, "run protocol tests and exit")
	flag.BoolVar(&doConfigure, "configure", false, "interactively write configuration and exit")
	flag.BoolVar(&doCheckConfig, "check-config", false, "validate configuration without changing it and exit")
	flag.Parse()
	if (doSelfTest && (doConfigure || doCheckConfig)) || (doConfigure && doCheckConfig) {
		log.Fatal("choose only one of --self-test, --configure, and --check-config")
	}
	if doSelfTest {
		if err := selfTest(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("self-test OK")
		return
	}
	if doConfigure {
		if _, err := interactiveConfig(configPath); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := loadConfig(configPath)
	if doCheckConfig {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("configuration OK")
		return
	}
	if err != nil {
		log.Printf("configuration %s unavailable or invalid: %v", configPath, err)
		cfg, err = interactiveConfig(configPath)
		if err != nil {
			log.Fatal(err)
		}
	}
	records, err := newStore(cfg.dataFile, cfg)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", cfg.tcpListen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("phicomm-s7d %s config=%s TCP=%s HTTP=%s data=%s records=%d fat=channel6", version, cfg.configPath, cfg.tcpListen, cfg.httpListen, cfg.dataFile, records.count())
	go func() {
		if err := serveProtocol(listener, cfg, records); err != nil {
			log.Fatal(err)
		}
	}()
	if err := serveWeb(cfg, records); err != nil {
		log.Fatal(err)
	}
}

const dashboardHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Phicomm S7</title><link rel="icon" href="/favicon.svg" type="image/svg+xml"><style>
:root{color-scheme:light dark;--bg:#f4f6f8;--card:#fff;--fg:#1d2733;--muted:#66717f;--line:#dce2e8;--blue:#3478f6;--orange:#f28c28;--danger:#d83b3b}*{box-sizing:border-box}body{margin:0;font:14px system-ui,sans-serif;background:var(--bg);color:var(--fg)}main{max-width:1120px;margin:auto;padding:22px}.head,.toolbar,.pager{display:flex;justify-content:space-between;align-items:center;gap:10px}.actions{display:flex;gap:8px}.iconbtn,.button,select,input{border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--fg);min-height:36px;padding:7px 10px}.iconbtn{width:38px;padding:7px;display:inline-grid;place-items:center;cursor:pointer}.iconbtn svg,.button svg{width:17px;height:17px;fill:currentColor}.button{display:inline-flex;align-items:center;gap:7px;text-decoration:none;cursor:pointer}.cards{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px;margin:16px 0}.card,.panel{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px}.big{font-size:30px;font-weight:700;margin-top:5px;white-space:nowrap}.muted{color:var(--muted)}sup{font-size:.7em;vertical-align:super}.footnote{margin:14px 2px 2px;color:var(--muted);font-size:12px;line-height:1.5}.footnote a{color:inherit}canvas{width:100%;height:300px}.panel-head{display:flex;justify-content:space-between;align-items:center;gap:10px;margin-bottom:10px}.section-title{font-size:24px;line-height:1.2}.filters{display:flex;flex-wrap:wrap;justify-content:flex-end;gap:7px;align-items:center;margin:0}.custom{display:none;gap:6px;align-items:center}.custom.visible{display:flex}.pager{flex-wrap:wrap;margin:10px 0}.pager .group,.pager .nav,.pagecontrols{display:flex;align-items:center;gap:6px}.pager .group,.pagecontrols{flex-wrap:wrap}.pager .pagesize{gap:4px}.pager .nav{flex-wrap:nowrap;white-space:nowrap}.pager button:disabled{opacity:.45}.datebox{display:inline-flex;align-items:center;position:relative;overflow:hidden;border-radius:8px}.datebox input[type=text]{width:142px;padding-right:45px}.datepick{position:absolute;right:1px;top:1px;bottom:1px;width:40px;min-height:0;padding:0;border:0;border-left:1px solid var(--line);border-radius:0 7px 7px 0;background:var(--card);pointer-events:none;display:grid;place-items:center}.datepick svg{width:16px;height:16px;fill:currentColor}.native-date{position:absolute;z-index:2;right:1px;top:11px;width:16px;height:14px;min-height:0;padding:0;border:0;opacity:0;cursor:pointer;transform:scale(2.5,2.4286);transform-origin:right center}table{width:100%;border-collapse:collapse;margin-top:8px}th,td{text-align:left;padding:8px;border-bottom:1px solid var(--line);white-space:nowrap}th:last-child,td:last-child{text-align:right}.scroll{overflow:auto}.trash{color:var(--danger);border:0;background:transparent;cursor:pointer;padding:5px}.trash svg{width:15px;height:15px;fill:currentColor}.empty{text-align:center;color:var(--muted);padding:30px}.language-title{display:flex;align-items:flex-start;gap:7px;line-height:1.4}.language-title svg{width:18px;height:18px;flex:none;fill:currentColor}.language-heading{display:flex;flex-wrap:wrap}.language-name{white-space:nowrap}.language-name:not(:last-child)::after{content:' · ';white-space:pre}.translation-warning{margin-top:-9px;padding:8px 10px;border-left:3px solid var(--orange);color:var(--muted);font-size:12px;line-height:1.45}dialog{border:1px solid var(--line);border-radius:12px;background:var(--card);color:var(--fg);min-width:min(360px,calc(100vw - 30px));padding:0}dialog::backdrop{background:#0008}.settings-head,.settings-body{padding:16px}.settings-body{display:grid;gap:16px}.settings-head{display:flex;justify-content:space-between;border-bottom:1px solid var(--line)}label{display:grid;gap:7px}.close{border:0;background:transparent;color:inherit;font-size:22px;cursor:pointer}@media(max-width:700px){main{padding:9px}.card,.panel{padding:10px}.cards{gap:6px}.big{font-size:clamp(17px,5vw,24px)}.card .muted{font-size:11px}canvas{height:250px}.button span{display:none}.head h1{font-size:21px;margin:10px 0}.section-title{font-size:20px}.panel-head{align-items:flex-start}.filters{max-width:70%}.pager{align-items:flex-start}.pager .group,.pager .nav,.pagecontrols{gap:4px}.pager .pagesize{gap:4px}.datebox input[type=text]{width:126px;padding-right:45px}}
@media(prefers-color-scheme:dark){:root{--bg:#11161c;--card:#18202a;--fg:#e7edf4;--muted:#9aa7b5;--line:#303b47}}</style></head><body><main>
<div class="head"><div><h1>Phicomm S7</h1><div class="muted" id="updated" data-i18n="loading"></div></div><div class="actions"><a class="button" href="/api/export.csv" id="export" title="CSV"><svg viewBox="0 0 384 512"><path d="M64 0C28.7 0 0 28.7 0 64v384c0 35.3 28.7 64 64 64h256c35.3 0 64-28.7 64-64V160H256c-17.7 0-32-14.3-32-32V0H64zm192 0v128h128L256 0zM64 288c0-17.7 14.3-32 32-32h32c17.7 0 32 14.3 32 32s-14.3 32-32 32h-16v32h16c17.7 0 32 14.3 32 32s-14.3 32-32 32H96c-17.7 0-32-14.3-32-32v-96zm128 0c0-17.7 14.3-32 32-32h48c17.7 0 32 14.3 32 32s-14.3 32-32 32h-16v64c0 17.7-14.3 32-32 32s-32-14.3-32-32v-96z"/></svg><span data-i18n="export"></span></a><button class="iconbtn" id="settingsButton" aria-label="Settings"><svg viewBox="0 0 512 512"><path d="M487.4 315.7l-42.6-24.6c2.1-11.5 3.2-23.2 3.2-35.1s-1.1-23.6-3.2-35.1l42.6-24.6c11.5-6.6 15.4-21.3 8.8-32.8l-32-55.4c-6.6-11.5-21.3-15.4-32.8-8.8l-42.6 24.6a192.4 192.4 0 0 0-60.8-35.1V40c0-13.3-10.7-24-24-24h-64c-13.3 0-24 10.7-24 24v49a192.4 192.4 0 0 0-60.8 35.1l-42.6-24.6c-11.5-6.6-26.2-2.7-32.8 8.8l-32 55.4c-6.6 11.5-2.7 26.2 8.8 32.8l42.6 24.6A194.3 194.3 0 0 0 64 256c0 11.9 1.1 23.6 3.2 35.1l-42.6 24.6c-11.5 6.6-15.4 21.3-8.8 32.8l32 55.4c6.6 11.5 21.3 15.4 32.8 8.8l42.6-24.6a192.4 192.4 0 0 0 60.8 35.1V472c0 13.3 10.7 24 24 24h64c13.3 0 24-10.7 24-24v-49a192.4 192.4 0 0 0 60.8-35.1l42.6 24.6c11.5 6.6 26.2 2.7 32.8-8.8l32-55.4c6.6-11.3 2.7-26-8.8-32.6zM256 336a80 80 0 1 1 0-160 80 80 0 1 1 0 160z"/></svg></button></div></div>
<section class="cards"><div class="card"><div class="muted" data-i18n="latestWeight"></div><div class="big" id="weight">—</div></div><div class="card"><div class="muted"><span data-i18n="latestFat"></span><sup>1</sup></div><div class="big" id="fat">—</div></div><div class="card"><div class="muted" data-i18n="latestZ6"></div><div class="big" id="z6">—</div></div></section>
<section class="panel"><div class="panel-head"><b class="section-title" data-i18n="combinedTrend"></b><div class="filters"><span data-i18n="chartRange"></span><select id="chartPeriod"><option value="1m" data-i18n="month1"></option><option value="3m" data-i18n="month3"></option><option value="6m" data-i18n="month6"></option><option value="1y" data-i18n="year1"></option><option value="custom" data-i18n="custom"></option></select><div class="custom" id="chartCustom"><span class="datebox"><input type="text" id="chartFrom"><button class="datepick" id="chartFromPick" aria-label="Calendar"><svg viewBox="0 0 448 512"><path d="M128 0c17.7 0 32 14.3 32 32v32h128V32c0-17.7 14.3-32 32-32s32 14.3 32 32v32h48c26.5 0 48 21.5 48 48v48H0v-48c0-26.5 21.5-48 48-48h48V32c0-17.7 14.3-32 32-32zM0 192h448v272c0 26.5-21.5 48-48 48H48c-26.5 0-48-21.5-48-48V192zm64 80v48h48v-48H64zm96 0v48h48v-48h-48zm96 0v48h48v-48h-48zm96 0v48h32v-48h-32zM64 368v48h48v-48H64zm96 0v48h48v-48h-48zm96 0v48h48v-48h-48z"/></svg></button><input class="native-date" type="date" id="chartFromNative"></span><span>–</span><span class="datebox"><input type="text" id="chartTo"><button class="datepick" id="chartToPick" aria-label="Calendar"><svg viewBox="0 0 448 512"><path d="M128 0c17.7 0 32 14.3 32 32v32h128V32c0-17.7 14.3-32 32-32s32 14.3 32 32v32h48c26.5 0 48 21.5 48 48v48H0v-48c0-26.5 21.5-48 48-48h48V32c0-17.7 14.3-32 32-32zM0 192h448v272c0 26.5-21.5 48-48 48H48c-26.5 0-48-21.5-48-48V192zm64 80v48h48v-48H64zm96 0v48h48v-48h-48zm96 0v48h48v-48h-48zm96 0v48h32v-48h-32zM64 368v48h48v-48H64zm96 0v48h48v-48h-48zm96 0v48h48v-48h-48z"/></svg></button><input class="native-date" type="date" id="chartToNative"></span></div></div></div><canvas id="combinedChart"></canvas></section>
<section class="panel" style="margin-top:12px"><b class="section-title" data-i18n="records"></b><div id="pagerTop"></div><div class="scroll"><table><thead><tr><th data-i18n="time"></th><th data-i18n="weight"></th><th data-i18n="fat"></th><th data-i18n="impedance"></th><th></th></tr></thead><tbody id="rows"></tbody></table></div><div id="empty" class="empty" hidden data-i18n="noData"></div><div id="pagerBottom"></div></section>
<footer class="footnote"><sup>1</sup> <span data-i18n="fatNote"></span> <a href="https://pubmed.ncbi.nlm.nih.gov/12540391/" target="_blank" rel="noopener noreferrer">Sun et al., 2003</a>.</footer>
</main><dialog id="settings"><div class="settings-head"><b data-i18n="settings"></b><button class="close" id="settingsClose">×</button></div><div class="settings-body"><label><span class="language-title"><svg viewBox="0 0 640 640" aria-hidden="true"><!--!Font Awesome Free 7.3.1 by @fontawesome - https://fontawesome.com License - https://fontawesome.com/license/free Copyright 2026 Fonticons, Inc.--><path d="M192 64C209.7 64 224 78.3 224 96L224 128L352 128C369.7 128 384 142.3 384 160C384 177.7 369.7 192 352 192L342.4 192L334 215.1C317.6 260.3 292.9 301.6 261.8 337.1C276 345.9 290.8 353.7 306.2 360.6L356.6 383L418.8 243C423.9 231.4 435.4 224 448 224C460.6 224 472.1 231.4 477.2 243L605.2 531C612.4 547.2 605.1 566.1 589 573.2C572.9 580.3 553.9 573.1 546.8 557L526.8 512L369.3 512L349.3 557C342.1 573.2 323.2 580.4 307.1 573.2C291 566 283.7 547.1 290.9 531L330.7 441.5L280.3 419.1C257.3 408.9 235.3 396.7 214.5 382.7C193.2 399.9 169.9 414.9 145 427.4L110.3 444.6C94.5 452.5 75.3 446.1 67.4 430.3C59.5 414.5 65.9 395.3 81.7 387.4L116.2 370.1C132.5 361.9 148 352.4 162.6 341.8C148.8 329.1 135.8 315.4 123.7 300.9L113.6 288.7C102.3 275.1 104.1 254.9 117.7 243.6C131.3 232.3 151.5 234.1 162.8 247.7L173 259.9C184.5 273.8 197.1 286.7 210.4 298.6C237.9 268.2 259.6 232.5 273.9 193.2L274.4 192L64.1 192C46.3 192 32 177.7 32 160C32 142.3 46.3 128 64 128L160 128L160 96C160 78.3 174.3 64 192 64zM448 334.8L397.7 448L498.3 448L448 334.8z"/></svg><span class="language-heading" id="languageHeading"></span></span><select id="language"><option value="auto"></option><option value="de"></option><option value="en"></option><option value="es"></option><option value="fr"></option><option value="ja"></option><option value="ko"></option><option value="pt"></option><option value="zh-Hans"></option><option value="zh-Hant"></option></select></label><div class="translation-warning" id="translationWarning" hidden><span id="translationWarningNative"></span><br><span lang="en">This language was machine-translated.</span></div><label><span data-i18n="dateFormat"></span><select id="dateFormat"><option value="yyyy-mm-dd">yyyy-mm-dd</option><option value="yyyy/mm/dd">yyyy/mm/dd</option><option value="dd/mm/yyyy">dd/mm/yyyy</option><option value="mm/dd/yyyy">mm/dd/yyyy</option><option value="yy/mm/dd">yy/mm/dd</option><option value="dd/mm/yy">dd/mm/yy</option><option value="mm/dd/yy">mm/dd/yy</option></select></label></div></dialog>
<script>
const $=id=>document.getElementById(id),T={
de:{autoName:'Automatisch',loading:'Laden…',updated:'Zuletzt aktualisiert: ',export:'CSV exportieren',latestWeight:'Gewicht',latestFat:'Körperfett',latestZ6:'Impedanz Kanal 6',fatNote:'Das Körperfett wird aus der Impedanz von Kanal 6 mit der BIA-Gleichung von Sun et al. geschätzt; der Wert wird nicht direkt von der Waage oder dem Server geliefert. Siehe',chartRange:'Diagrammzeitraum',month1:'Letzter Monat',month3:'Letzte 3 Monate',month6:'Letzte 6 Monate',year1:'Letztes Jahr',custom:'Benutzerdefiniert',combinedTrend:'Trend',records:'Messungen',time:'Datum und Uhrzeit',weight:'Gewicht',fat:'Körperfett',impedance:'Impedanz 1–6 (Ω)',noData:'Keine Messungen in diesem Zeitraum',settings:'Einstellungen',language:'Sprache',dateFormat:'Datumsformat',deleteConfirm:'Diese Messung löschen?',from:'Von',to:'Bis',previous:'Zurück',next:'Weiter',page:'Seite {page} / {pages}, {total} Einträge',pageSuffix:' /Seite',rangeTooLong:'Der Diagrammzeitraum darf höchstens ein Jahr betragen',invalidDate:'Ungültiges Datumsformat'},
en:{autoName:'Automatic',loading:'Loading…',updated:'Last updated: ',export:'Export CSV',latestWeight:'Weight',latestFat:'Body fat',latestZ6:'Ch.6 impedance',fatNote:'Body fat is estimated from Ch.6 impedance using the BIA equation by Sun et al.; it is not a body-fat value returned directly by the scale or server. See',chartRange:'Chart range',month1:'Last month',month3:'Last 3 months',month6:'Last 6 months',year1:'Last year',custom:'Custom dates',combinedTrend:'Trend',records:'Measurements',time:'Date and time',weight:'Weight',fat:'Body fat',impedance:'Impedance 1–6 (Ω)',noData:'No measurements in this range',settings:'Settings',language:'Language',dateFormat:'Date format',deleteConfirm:'Delete this measurement?',from:'From',to:'To',previous:'Previous',next:'Next',page:'Page {page} / {pages}, {total} records',pageSuffix:' /page',rangeTooLong:'Chart date range cannot exceed one year',invalidDate:'Invalid date format'},
es:{autoName:'Automático',loading:'Cargando…',updated:'Última actualización: ',export:'Exportar CSV',latestWeight:'Peso',latestFat:'Grasa corporal',latestZ6:'Impedancia canal 6',fatNote:'La grasa corporal se estima a partir de la impedancia del canal 6 mediante la ecuación BIA de Sun et al.; no es un valor enviado directamente por la báscula ni por el servidor. Véase',chartRange:'Intervalo del gráfico',month1:'Último mes',month3:'Últimos 3 meses',month6:'Últimos 6 meses',year1:'Último año',custom:'Fechas personalizadas',combinedTrend:'Tendencia',records:'Mediciones',time:'Fecha y hora',weight:'Peso',fat:'Grasa corporal',impedance:'Impedancia 1–6 (Ω)',noData:'No hay mediciones en este intervalo',settings:'Ajustes',language:'Idioma',dateFormat:'Formato de fecha',deleteConfirm:'¿Eliminar esta medición?',from:'Desde',to:'Hasta',previous:'Anterior',next:'Siguiente',page:'Página {page} / {pages}, {total} registros',pageSuffix:' /página',rangeTooLong:'El intervalo del gráfico no puede superar un año',invalidDate:'Formato de fecha no válido'},
fr:{autoName:'Automatique',loading:'Chargement…',updated:'Dernière mise à jour : ',export:'Exporter en CSV',latestWeight:'Poids',latestFat:'Masse grasse',latestZ6:'Impédance canal 6',fatNote:'La masse grasse est estimée à partir de l’impédance du canal 6 avec l’équation BIA de Sun et al. ; cette valeur n’est pas renvoyée directement par la balance ou le serveur. Voir',chartRange:'Période du graphique',month1:'Dernier mois',month3:'3 derniers mois',month6:'6 derniers mois',year1:'Dernière année',custom:'Dates personnalisées',combinedTrend:'Tendance',records:'Mesures',time:'Date et heure',weight:'Poids',fat:'Masse grasse',impedance:'Impédance 1–6 (Ω)',noData:'Aucune mesure sur cette période',settings:'Paramètres',language:'Langue',dateFormat:'Format de date',deleteConfirm:'Supprimer cette mesure ?',from:'Du',to:'Au',previous:'Précédent',next:'Suivant',page:'Page {page} / {pages}, {total} mesures',pageSuffix:' /page',rangeTooLong:'La période du graphique ne peut pas dépasser un an',invalidDate:'Format de date incorrect'},
ja:{autoName:'自動',loading:'読み込み中…',updated:'最終更新：',export:'CSVをエクスポート',latestWeight:'体重',latestFat:'体脂肪率',latestZ6:'第6チャンネルのインピーダンス',fatNote:'体脂肪率は第6チャンネルのインピーダンスとSunらのBIA式から推定した値で、体重計またはサーバーから直接返された測定値ではありません。参照：',chartRange:'グラフ期間',month1:'過去1か月',month3:'過去3か月',month6:'過去6か月',year1:'過去1年',custom:'期間を指定',combinedTrend:'推移',records:'測定記録',time:'日時',weight:'体重',fat:'体脂肪率',impedance:'インピーダンス 1–6 (Ω)',noData:'指定期間に測定記録がありません',settings:'設定',language:'言語',dateFormat:'日付形式',deleteConfirm:'この測定記録を削除しますか？',from:'開始',to:'終了',previous:'前へ',next:'次へ',page:'{page} / {pages} ページ、全 {total} 件',pageSuffix:' /ページ',rangeTooLong:'グラフの期間は1年以内にしてください',invalidDate:'日付形式が正しくありません'},
ko:{autoName:'자동',loading:'불러오는 중…',updated:'마지막 업데이트: ',export:'CSV 내보내기',latestWeight:'체중',latestFat:'체지방률',latestZ6:'채널 6 임피던스',fatNote:'체지방률은 채널 6 임피던스와 Sun 등의 BIA 방정식으로 추정한 값이며, 체중계나 서버가 직접 반환한 측정값이 아닙니다. 참고:',chartRange:'차트 기간',month1:'최근 1개월',month3:'최근 3개월',month6:'최근 6개월',year1:'최근 1년',custom:'사용자 지정 날짜',combinedTrend:'추이',records:'측정 기록',time:'날짜 및 시간',weight:'체중',fat:'체지방률',impedance:'임피던스 1–6 (Ω)',noData:'선택한 기간에 측정 기록이 없습니다',settings:'설정',language:'언어',dateFormat:'날짜 형식',deleteConfirm:'이 측정 기록을 삭제하시겠습니까?',from:'시작',to:'종료',previous:'이전',next:'다음',page:'{page} / {pages}페이지, 총 {total}건',pageSuffix:' /페이지',rangeTooLong:'차트 기간은 1년을 초과할 수 없습니다',invalidDate:'잘못된 날짜 형식입니다'},
pt:{autoName:'Automático',loading:'Carregando…',updated:'Última atualização: ',export:'Exportar CSV',latestWeight:'Peso',latestFat:'Gordura corporal',latestZ6:'Impedância do canal 6',fatNote:'A gordura corporal é estimada pela impedância do canal 6 usando a equação BIA de Sun et al.; não é um valor retornado diretamente pela balança ou pelo servidor. Consulte',chartRange:'Período do gráfico',month1:'Último mês',month3:'Últimos 3 meses',month6:'Últimos 6 meses',year1:'Último ano',custom:'Datas personalizadas',combinedTrend:'Tendência',records:'Medições',time:'Data e hora',weight:'Peso',fat:'Gordura corporal',impedance:'Impedância 1–6 (Ω)',noData:'Nenhuma medição neste período',settings:'Configurações',language:'Idioma',dateFormat:'Formato de data',deleteConfirm:'Excluir esta medição?',from:'De',to:'Até',previous:'Anterior',next:'Próxima',page:'Página {page} / {pages}, {total} registros',pageSuffix:' /página',rangeTooLong:'O período do gráfico não pode exceder um ano',invalidDate:'Formato de data inválido'},
'zh-Hans':{autoName:'自动',loading:'读取中…',updated:'最后更新：',export:'导出 CSV',latestWeight:'体重',latestFat:'体脂',latestZ6:'第六路阻抗',fatNote:'体脂根据第六路阻抗及 Sun 等人的 BIA 方程估算，并非体脂秤或服务器直接返回的测量值。参见',chartRange:'图表范围',month1:'近一个月',month3:'近三个月',month6:'近半年',year1:'近一年',custom:'自定义日期',combinedTrend:'趋势',records:'测量记录',time:'日期时间',weight:'体重',fat:'体脂',impedance:'阻抗 1–6 (Ω)',noData:'所选范围内没有记录',settings:'设置',language:'语言',dateFormat:'日期格式',deleteConfirm:'确定删除这条记录吗？',from:'从',to:'至',previous:'上一页',next:'下一页',page:'第 {page} / {pages} 页，共 {total} 条',pageSuffix:' /页',rangeTooLong:'图表日期范围不能超过一年',invalidDate:'日期格式不正确'},
'zh-Hant':{autoName:'自動',loading:'載入中…',updated:'最後更新：',export:'匯出 CSV',latestWeight:'體重',latestFat:'體脂',latestZ6:'第六路阻抗',fatNote:'體脂是根據第六路阻抗及 Sun 等人的 BIA 方程推算，並非由體脂磅或伺服器直接傳回的量度結果。請參閱',chartRange:'圖表時段',month1:'過去一個月',month3:'過去三個月',month6:'過去半年',year1:'過去一年',custom:'自訂日期',combinedTrend:'走勢',records:'量度記錄',time:'日期及時間',weight:'體重',fat:'體脂',impedance:'阻抗 1–6 (Ω)',noData:'所選時段內沒有記錄',settings:'設定',language:'語言',dateFormat:'日期格式',deleteConfirm:'確定刪除這項記錄？',from:'由',to:'至',previous:'上一頁',next:'下一頁',page:'第 {page} / {pages} 頁，共 {total} 項',pageSuffix:' /頁',rangeTooLong:'圖表日期範圍不可超過一年',invalidDate:'日期格式不正確'}
};
const nativeLanguageNames={de:'Deutsch',en:'English',es:'Español',fr:'Français',ja:'日本語',ko:'한국어',pt:'Português','zh-Hans':'简体中文','zh-Hant':'繁體中文'};
const machineWarnings={de:'Diese Sprache wurde maschinell übersetzt.',es:'Este idioma ha sido traducido automáticamente.',fr:'Cette langue a été traduite automatiquement.',ja:'この言語は機械翻訳されています。',ko:'이 언어는 기계 번역되었습니다.',pt:'Este idioma foi traduzido automaticamente.'};
let pref=localStorage.getItem('s7-language')||'auto',datePref=localStorage.getItem('s7-date-format')||'yyyy-mm-dd',chartData=[],recordResult={items:[],total:0,page:1,page_size:50},recordPage=1,recordSize=Number(localStorage.getItem('s7-page-size'))||50;
if(pref==='zh'){pref='zh-Hans';localStorage.setItem('s7-language',pref)}
function autoLang(){const value=(navigator.languages?.[0]||navigator.language||'en').toLowerCase();if(value.startsWith('zh'))return /(?:hant|tw|hk|mo)/.test(value)?'zh-Hant':'zh-Hans';const base=value.split('-')[0];return T[base]?base:'en'}
function lang(){return pref==='auto'?autoLang():(T[pref]?pref:'en')}
function tr(k){return T[lang()][k]||T.en[k]||k}
function displayLocale(){return lang()==='zh-Hant'?'zh-HK':lang()}
function languageLabel(code){if(code==='auto')return 'auto: '+tr('autoName');const native=nativeLanguageNames[code];let translated=native;try{translated=new Intl.DisplayNames([displayLocale()],{type:'language'}).of(code)||native}catch(e){}return code+': '+native+(translated.toLocaleLowerCase(displayLocale())===native.toLocaleLowerCase(displayLocale())?'':'('+translated+')')}
function languageHeading(){const entries=[],seen=new Set;for(const code of Object.keys(nativeLanguageNames).sort()){const label=T[code].language;if(!seen.has(label)){entries.push({code,label});seen.add(label)}}const current=T[lang()].language,index=entries.findIndex(e=>e.label===current);if(index>0)entries.unshift(entries.splice(index,1)[0]);return entries.map(e=>e.label)}
function renderLanguageHeading(){const heading=$('languageHeading');heading.replaceChildren(...languageHeading().map(label=>{const item=document.createElement('span');item.className='language-name';item.textContent=label;return item}))}
function isoDate(d){const y=d.getFullYear(),m=String(d.getMonth()+1).padStart(2,'0'),day=String(d.getDate()).padStart(2,'0');return y+'-'+m+'-'+day}function fmt(v,n=2){return v==null?'—':Number(v).toFixed(n)}
function translate(){const active=lang();document.documentElement.lang=displayLocale();document.querySelectorAll('[data-i18n]').forEach(e=>e.textContent=tr(e.dataset.i18n));renderLanguageHeading();$('language').querySelectorAll('option').forEach(e=>e.textContent=languageLabel(e.value));$('language').value=pref;const warning=machineWarnings[active];$('translationWarning').hidden=!warning;$('translationWarningNative').textContent=warning||'';$('translationWarningNative').lang=displayLocale();$('dateFormat').value=datePref;refreshDateFields();renderRecords();renderCharts()}
function formatDate(iso){if(!iso)return'';const p=iso.slice(0,10).split('-'),y=p[0],m=p[1],d=p[2];if(datePref==='yyyy/mm/dd')return y+'/'+m+'/'+d;if(datePref==='dd/mm/yyyy')return d+'/'+m+'/'+y;if(datePref==='mm/dd/yyyy')return m+'/'+d+'/'+y;if(datePref==='yy/mm/dd')return y.slice(2)+'/'+m+'/'+d;if(datePref==='dd/mm/yy')return d+'/'+m+'/'+y.slice(2);if(datePref==='mm/dd/yy')return m+'/'+d+'/'+y.slice(2);return y+'-'+m+'-'+d}
function parseDate(value){value=value.trim();let y,m,d;if(datePref==='yyyy-mm-dd')[y,m,d]=value.split('-');else{const p=value.split('/');if(datePref==='yyyy/mm/dd'||datePref==='yy/mm/dd')[y,m,d]=p;else if(datePref==='dd/mm/yyyy'||datePref==='dd/mm/yy')[d,m,y]=p;else [m,d,y]=p;if((datePref==='yy/mm/dd'||datePref==='dd/mm/yy'||datePref==='mm/dd/yy')&&y)y=String(Math.floor(new Date().getFullYear()/100))+String(y).padStart(2,'0')}if(!y||!m||!d)return'';const iso=String(y).padStart(4,'0')+'-'+String(m).padStart(2,'0')+'-'+String(d).padStart(2,'0'),test=new Date(iso+'T00:00:00');return !isNaN(test)&&isoDate(test)===iso?iso:''}
function formatDateTime(value){return value?formatDate(value)+' '+value.slice(11,16):''}
function setDateField(id,iso){const e=$(id);if(!e)return;e.dataset.iso=iso||'';e.value=formatDate(iso);const native=$(id+'Native');if(native)native.value=iso||''}
function getDateField(id){const e=$(id);if(!e)return'';const parsed=parseDate(e.value);if(parsed){e.dataset.iso=parsed;return parsed}return e.dataset.iso||''}
function bindDateField(id,onchange){const text=$(id),native=$(id+'Native'),button=$(id+'Pick');text.placeholder=datePref;text.onchange=()=>{const parsed=parseDate(text.value);if(!parsed&&text.value){alert(tr('invalidDate'));setDateField(id,text.dataset.iso||'');return}setDateField(id,parsed);onchange()};native.onchange=()=>{setDateField(id,native.value);onchange()};button.onclick=e=>{e.preventDefault();if(native.showPicker)native.showPicker();else native.click()}}
function refreshDateFields(){document.querySelectorAll('.datebox input[type=text]').forEach(e=>{e.placeholder=datePref;setDateField(e.id,e.dataset.iso||parseDate(e.value))})}
function chartDates(){const p=$('chartPeriod').value,to=new Date(),from=new Date(to);if(p==='1m')from.setMonth(from.getMonth()-1);else if(p==='3m')from.setMonth(from.getMonth()-3);else if(p==='6m')from.setMonth(from.getMonth()-6);else if(p==='1y')from.setFullYear(from.getFullYear()-1);else return {from:getDateField('chartFrom'),to:getDateField('chartTo')};return {from:isoDate(from),to:isoDate(to)}}
function validateChartCustom(){const from=getDateField('chartFrom'),to=getDateField('chartTo');if(!from||!to)return false;const a=new Date(from+'T00:00:00'),b=new Date(to+'T00:00:00');if(b<a||b-a>366*86400000){alert(tr('rangeTooLong'));return false}return true}
function dailyChartData(data){const days=new Map;for(const v of data){const day=v.received_at.slice(0,10),a=days.get(day)||{received_at:day,weightSum:0,weightCount:0,fatSum:0,fatCount:0};if(v.weight_kg!=null){a.weightSum+=Number(v.weight_kg);a.weightCount++}if(v.fat_percent!=null){a.fatSum+=Number(v.fat_percent);a.fatCount++}days.set(day,a)}return [...days.values()].sort((a,b)=>a.received_at.localeCompare(b.received_at)).map(a=>({received_at:a.received_at,weight_kg:a.weightCount?a.weightSum/a.weightCount:null,fat_percent:a.fatCount?a.fatSum/a.fatCount:null}))}
function renderCharts(){const c=$('combinedChart'),d=devicePixelRatio||1,w=Math.max(280,c.clientWidth),h=c.clientHeight;c.width=w*d;c.height=h*d;const x=c.getContext('2d');x.scale(d,d);x.clearRect(0,0,w,h);const p=dailyChartData(chartData);if(!p.length)return;const bounds=key=>{const a=p.map(v=>v[key]).filter(v=>v!=null);if(!a.length)return null;const min=Math.min(...a),max=Math.max(...a),pad=(max-min||1)*.12;return [min-pad,max+pad]},wb=bounds('weight_kg'),fb=bounds('fat_percent');if(!wb&&!fb)return;const left=48,right=48,top=25,bottom=42,pw=w-left-right,ph=h-top-bottom,fg=getComputedStyle(document.body).color,line=getComputedStyle(document.documentElement).getPropertyValue('--line');x.font='11px system-ui';x.lineWidth=1;for(let i=0;i<=4;i++){const py=top+ph*i/4;x.strokeStyle=line;x.beginPath();x.moveTo(left,py);x.lineTo(w-right,py);x.stroke();x.fillStyle='#3478f6';if(wb)x.fillText((wb[1]-(wb[1]-wb[0])*i/4).toFixed(1),3,py+4);x.fillStyle='#f28c28';if(fb){const label=(fb[1]-(fb[1]-fb[0])*i/4).toFixed(1);x.fillText(label,w-right+5,py+4)}}const ticks=Math.min(4,p.length-1);x.fillStyle=fg;for(let i=0;i<=ticks;i++){const idx=Math.round((p.length-1)*i/Math.max(1,ticks)),px=left+pw*idx/Math.max(1,p.length-1);x.strokeStyle=line;x.beginPath();x.moveTo(px,top);x.lineTo(px,h-bottom);x.stroke();x.save();x.translate(px-3,h-5);x.rotate(-.35);x.fillText(formatDate(p[idx].received_at),0,0);x.restore()}const series=(key,b,color)=>{if(!b)return;x.strokeStyle=color;x.lineWidth=2;x.beginPath();let started=false;p.forEach((v,i)=>{if(v[key]==null){started=false;return}const px=left+pw*i/Math.max(1,p.length-1),py=top+(b[1]-v[key])*ph/(b[1]-b[0]);if(started)x.lineTo(px,py);else{x.moveTo(px,py);started=true}});x.stroke()};series('weight_kg',wb,'#3478f6');series('fat_percent',fb,'#f28c28');x.fillStyle='#3478f6';x.fillText('kg',3,13);x.fillStyle='#f28c28';x.fillText('%',w-right+5,13)}
async function loadLatest(){const r=await fetch('/api/latest',{cache:'no-store'});if(!r.ok)return;const v=await r.json();$('weight').textContent=fmt(v.weight_kg)+' kg';$('fat').textContent=fmt(v.fat_percent)+' %';$('z6').textContent=fmt(v.impedance_ohm?.[5],1)+' Ω';$('updated').textContent=tr('updated')+formatDateTime(v.received_at)}
async function loadCharts(){const p=$('chartPeriod').value;$('chartCustom').classList.toggle('visible',p==='custom');if(p==='custom'&&!validateChartCustom())return;const d=chartDates(),q=new URLSearchParams(d);const r=await fetch('/api/chart?'+q,{cache:'no-store'});if(r.ok){chartData=await r.json();renderCharts()}}
function recordQuery(){const q=new URLSearchParams({page:String(recordPage),page_size:String(recordSize)}),from=getDateField('recordFromTop'),to=getDateField('recordToTop');if(from)q.set('from',from);if(to)q.set('to',to);return q}
function dateBox(id){return '<span class="datebox"><input type="text" id="'+id+'"><button class="datepick" id="'+id+'Pick" aria-label="Calendar"><svg viewBox="0 0 448 512"><path d="M128 0c17.7 0 32 14.3 32 32v32h128V32c0-17.7 14.3-32 32-32s32 14.3 32 32v32h48c26.5 0 48 21.5 48 48v48H0v-48c0-26.5 21.5-48 48-48h48V32c0-17.7 14.3-32 32-32zM0 192h448v272c0 26.5-21.5 48-48 48H48c-26.5 0-48-21.5-48-48V192zm64 80v48h48v-48H64zm96 0v48h48v-48h-48zm96 0v48h48v-48h-48zm96 0v48h32v-48h-32zM64 368v48h48v-48H64zm96 0v48h48v-48h-48zm96 0v48h48v-48h-48z"/></svg></button><input class="native-date" type="date" id="'+id+'Native"></span>'}
function pagerHTML(place){return '<div class="pager"><div class="group"><span>'+tr('from')+'</span>'+dateBox('recordFrom'+place)+'<span>'+tr('to')+'</span>'+dateBox('recordTo'+place)+'</div><div class="pagecontrols"><div class="group pagesize"><select id="pageSize'+place+'"><option>50</option><option>100</option><option>200</option><option>500</option></select><span>'+tr('pageSuffix')+'</span></div><div class="nav"><button class="button" id="prev'+place+'">'+tr('previous')+'</button><span id="pageInfo'+place+'"></span><button class="button" id="next'+place+'">'+tr('next')+'</button></div></div></div>'}
function bindPager(place){const other=place==='Top'?'Bottom':'Top';$('pageSize'+place).value=String(recordSize);$('pageSize'+place).onchange=()=>{recordSize=Number($('pageSize'+place).value);$('pageSize'+other).value=String(recordSize);localStorage.setItem('s7-page-size',recordSize);recordPage=1;loadRecords()};for(const field of ['From','To'])bindDateField('record'+field+place,()=>{setDateField('record'+field+other,getDateField('record'+field+place));recordPage=1;loadRecords()});$('prev'+place).onclick=()=>{if(recordPage>1){recordPage--;loadRecords()}};$('next'+place).onclick=()=>{const pages=Math.max(1,Math.ceil(recordResult.total/recordSize));if(recordPage<pages){recordPage++;loadRecords()}}}
function rebuildPagers(){const from=getDateField('recordFromTop'),to=getDateField('recordToTop');$('pagerTop').innerHTML=pagerHTML('Top');$('pagerBottom').innerHTML=pagerHTML('Bottom');bindPager('Top');bindPager('Bottom');for(const p of ['Top','Bottom']){setDateField('recordFrom'+p,from);setDateField('recordTo'+p,to)}}
function renderRecords(){const items=recordResult.items||[],pages=Math.max(1,Math.ceil(recordResult.total/recordSize)),message=tr('page').replace('{page}',recordPage).replace('{pages}',pages).replace('{total}',recordResult.total);for(const p of ['Top','Bottom']){if($('pageInfo'+p)){$('pageInfo'+p).textContent=message;$('prev'+p).disabled=recordPage<=1;$('next'+p).disabled=recordPage>=pages}}$('empty').hidden=items.length>0;$('rows').replaceChildren(...items.map(v=>{const row=document.createElement('tr');[formatDateTime(v.received_at),fmt(v.weight_kg)+' kg',fmt(v.fat_percent)+' %',(v.impedance_ohm||[]).map(n=>fmt(n,1)).join(' / ')].forEach(text=>{const cell=document.createElement('td');cell.textContent=text;row.append(cell)});const action=document.createElement('td'),button=document.createElement('button');button.className='trash';button.title='Delete';button.innerHTML='<svg viewBox="0 0 448 512"><path d="M135.2 17.7L128 32H32C14.3 32 0 46.3 0 64S14.3 96 32 96h384c17.7 0 32-14.3 32-32s-14.3-32-32-32h-96l-7.2-14.3A32 32 0 0 0 284.2 0H163.8a32 32 0 0 0-28.6 17.7zM53.2 467c1.7 25.3 22.7 45 48 45h245.6c25.3 0 46.3-19.7 48-45L416 128H32l21.2 339z"/></svg>';button.onclick=()=>remove(v.id);action.append(button);row.append(action);return row}));const q=recordQuery();q.delete('page');q.delete('page_size');$('export').href='/api/export.csv?'+q}
async function loadRecords(){const r=await fetch('/api/measurements?'+recordQuery(),{cache:'no-store'});if(r.ok){recordResult=await r.json();renderRecords()}}
async function remove(id){if(!confirm(tr('deleteConfirm')))return;const r=await fetch('/api/measurements/'+encodeURIComponent(id),{method:'DELETE'});if(r.ok){await Promise.all([loadLatest(),loadCharts(),loadRecords()])}}
async function refresh(){await Promise.all([loadLatest(),loadCharts(),loadRecords()])}
$('chartPeriod').value=localStorage.getItem('s7-chart-period')||'3m';const now=new Date(),yearAgo=new Date(now);yearAgo.setFullYear(yearAgo.getFullYear()-1);setDateField('chartFrom',isoDate(yearAgo));setDateField('chartTo',isoDate(now));bindDateField('chartFrom',loadCharts);bindDateField('chartTo',loadCharts);$('chartPeriod').onchange=()=>{localStorage.setItem('s7-chart-period',$('chartPeriod').value);loadCharts()};rebuildPagers();$('settingsButton').onclick=()=>$('settings').showModal();$('settingsClose').onclick=()=>$('settings').close();$('language').onchange=()=>{pref=$('language').value;localStorage.setItem('s7-language',pref);translate();rebuildPagers();renderRecords()};$('dateFormat').onchange=()=>{datePref=$('dateFormat').value;localStorage.setItem('s7-date-format',datePref);refreshDateFields();renderRecords();renderCharts()};translate();refresh();setInterval(refresh,15000);addEventListener('resize',renderCharts);
</script></body></html>`
