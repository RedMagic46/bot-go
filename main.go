package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	nethttp "net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/joho/godotenv"
)

// Embed frontend static files
//
//go:embed static/*
var staticFS embed.FS

var (
	URL_JADWAL = "https://infokhs.umm.ac.id/jadwal-kuliah"
	USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// Data Models
type Profile struct {
	NIM              string    `json:"nim"`
	Name             string    `json:"name"`
	CookiesJSON      string    `json:"cookies_json"`
	Password         string    `json:"password"` // base64 encoded
	WAToken          string    `json:"wa_token"`
	WAGroup          string    `json:"wa_group"`
	IsActive         bool      `json:"is_active"`
	LastRunStatus    string    `json:"last_run_status"` // "success", "failed", "no_link", "expired", "needs_cookie", "refreshing", "paused", or ""
	LastRunTime      time.Time `json:"last_run_time"`
	LastRunMsg       string    `json:"last_run_msg"`
	FailedCount      int       `json:"failed_count"`
	CookieUpdatedAt  time.Time `json:"cookie_updated_at"` // kapan cookie terakhir di-refresh/di-push
	Paused           bool      `json:"paused"`            // dihentikan sementara sampai cookie baru diberikan
	PauseReason      string    `json:"pause_reason"`
	FailureAlertSent bool      `json:"failure_alert_sent"` // pengingat WA sudah dikirim untuk episode gagal ini
}

type GlobalConfig struct {
	WAToken       string `json:"wa_token"`
	WAGroup       string `json:"wa_group"`
	WAAlertNumber string `json:"wa_alert_number"` // nomor WA admin untuk pengingat 10x gagal
	CheckInterval int    `json:"check_interval"`  // in minutes
	StartHour     int    `json:"start_hour"`
	EndHour       int    `json:"end_hour"`
}

type CookieItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type CookieJSON struct {
	UserAgent string       `json:"user_agent"`
	Cookies   []CookieItem `json:"cookies"`
}

type CourseSuccess struct {
	CourseName string
	TimeInfo   string
}

type SuccessRecord struct {
	NIM        string
	Name       string
	CourseName string
	TimeInfo   string
	WAToken    string
	WAGroup    string
}

// In-Memory Database & Mutexes
var (
	studentProfilesMutex sync.RWMutex
	studentProfiles      []Profile

	configMutex  sync.RWMutex
	globalConfig = GlobalConfig{
		CheckInterval: 5,
		StartHour:     7,
		EndHour:       21,
	}
)

type CachedClient struct {
	Client    tls_client.HttpClient
	UserAgent string
}

var (
	clientCache   = make(map[string]CachedClient)
	clientCacheMu sync.Mutex
)

// Log System
type LogMessage struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"` // "info", "success", "warning", "error", "system"
	Message   string    `json:"message"`
}

var (
	logCache   []LogMessage
	logMutex   sync.Mutex
	listeners  = make(map[chan LogMessage]bool)
	listenerMu sync.Mutex
)

func AddLog(level, format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	logMsg := LogMessage{
		Timestamp: time.Now(),
		Level:     level,
		Message:   msg,
	}

	// Print to Stdout
	prefix := "ℹ️ "
	switch level {
	case "success":
		prefix = "🎉 "
	case "warning":
		prefix = "⚠️ "
	case "error":
		prefix = "❌ "
	case "system":
		prefix = "⚙️ "
	}
	fmt.Printf("[%s] %s%s\n", logMsg.Timestamp.Format("15:04:05"), prefix, msg)

	logMutex.Lock()
	logCache = append(logCache, logMsg)
	if len(logCache) > 500 { // Keep last 500 logs
		logCache = logCache[1:]
	}
	logMutex.Unlock()

	// Broadcast to SSE clients
	listenerMu.Lock()
	for ch := range listeners {
		select {
		case ch <- logMsg:
		default:
		}
	}
	listenerMu.Unlock()
}

// Data directory helper
//
// Di Azure App Service (custom container) folder /app bersifat sementara.
// Set env DATA_DIR=/home/data agar profiles.json & config.json tersimpan permanen.
func dataPath(name string) string {
	dir := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if dir == "" {
		return name
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		AddLog("warning", "Gagal membuat DATA_DIR '%s': %v (memakai folder saat ini)", dir, err)
		return name
	}
	return filepath.Join(dir, name)
}

// readDataFile membaca file dari DATA_DIR. Jika belum ada, coba migrasi
// otomatis dari folder aplikasi (lokasi lama) supaya data tidak hilang.
func readDataFile(name string) ([]byte, error) {
	target := dataPath(name)
	data, err := os.ReadFile(target)
	if err == nil {
		return data, nil
	}
	if target != name {
		if legacy, legacyErr := os.ReadFile(name); legacyErr == nil {
			if writeErr := os.WriteFile(target, legacy, 0o644); writeErr == nil {
				AddLog("system", "Data '%s' dimigrasikan ke DATA_DIR.", name)
			}
			return legacy, nil
		}
	}
	return nil, err
}

// Database Operations
func loadData() {
	// Try loading config
	cfgData, err := readDataFile("config.json")
	if err == nil {
		json.Unmarshal(cfgData, &globalConfig)
	} else {
		// Migrate legacy env values
		globalConfig.WAToken = os.Getenv("WA_TOKEN")
		globalConfig.WAGroup = os.Getenv("WA_GROUP")
		saveConfig()
	}

	// Try loading profiles
	profData, err := readDataFile("profiles.json")
	if err == nil {
		json.Unmarshal(profData, &studentProfiles)
	} else {
		// Migrate legacy env single NIM profile
		legacyNIM := os.Getenv("NIM")
		if legacyNIM != "" {
			legacyCookies := os.Getenv("COOKIES_JSON")
			if legacyCookies == "" {
				cookieFile := filepath.Join(".", "cookies_umm.json")
				if _, err := os.Stat(cookieFile); os.IsNotExist(err) {
					cookieFile = filepath.Join("..", "cookies_umm.json")
				}
				data, err := os.ReadFile(cookieFile)
				if err == nil {
					legacyCookies = string(data)
				}
			}
			if legacyCookies != "" {
				studentProfiles = append(studentProfiles, Profile{
					NIM:         legacyNIM,
					Name:        "Default Profile (Migrated)",
					CookiesJSON: legacyCookies,
					IsActive:    true,
				})
				saveStudentProfiles()
				AddLog("system", "Berhasil migrasi profil bawaan Env dari NIM %s.", legacyNIM)
			}
		}
	}
}

func saveStudentProfiles() {
	data, _ := json.MarshalIndent(studentProfiles, "", "  ")
	os.WriteFile(dataPath("profiles.json"), data, 0o644)
}

func saveConfig() {
	data, _ := json.MarshalIndent(globalConfig, "", "  ")
	os.WriteFile(dataPath("config.json"), data, 0o644)
}

// Cookie Helper
func parseCookies(cookieInput string) (string, string) {
	var userAgent = USER_AGENT
	var cookieString string

	cookieInput = strings.TrimSpace(cookieInput)
	if cookieInput == "" {
		return "", userAgent
	}

	// Try parsing CookieJSON
	var cj CookieJSON
	if err := json.Unmarshal([]byte(cookieInput), &cj); err == nil && len(cj.Cookies) > 0 {
		if cj.UserAgent != "" {
			userAgent = cj.UserAgent
		}
		var pairs []string
		for _, c := range cj.Cookies {
			pairs = append(pairs, c.Name+"="+c.Value)
		}
		cookieString = strings.Join(pairs, "; ")
		return cookieString, userAgent
	}

	// Try parsing direct array of CookieItem
	var cl []CookieItem
	if err := json.Unmarshal([]byte(cookieInput), &cl); err == nil && len(cl) > 0 {
		var pairs []string
		for _, c := range cl {
			pairs = append(pairs, c.Name+"="+c.Value)
		}
		cookieString = strings.Join(pairs, "; ")
		return cookieString, userAgent
	}

	// Try parsing raw Header string
	if strings.Contains(cookieInput, "=") {
		return cookieInput, userAgent
	}

	return "", userAgent
}

// WhatsApp sender
func sendWhatsApp(message string, waToken, waGroup string) {
	if waToken == "" || waGroup == "" {
		AddLog("info", "Pemberitahuan WhatsApp diabaikan (token/group kosong)")
		return
	}

	client := &nethttp.Client{}
	data := url.Values{}
	data.Set("target", waGroup)
	data.Set("message", message)
	data.Set("delay", "1")

	req, err := nethttp.NewRequest("POST", "https://api.fonnte.com/send", strings.NewReader(data.Encode()))
	if err != nil {
		AddLog("error", "Gagal membuat request WA: %v", err)
		return
	}
	req.Header.Set("Authorization", waToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		AddLog("error", "Gagal mengirim WhatsApp: %v", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		AddLog("error", "Pesan WA terkirim, tetapi gagal membaca respon: %v", err)
		return
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err == nil {
		if status, ok := result["status"].(bool); ok && status {
			AddLog("success", "Notifikasi WhatsApp terkirim ke %s.", waGroup)
		} else {
			AddLog("error", "WhatsApp Fonnte error: %s", string(body))
		}
	} else {
		AddLog("warning", "Respon mentah WA: %s", string(body))
	}
}

// Request Helper
func makeRequest(client tls_client.HttpClient, targetUrl string, cookieHeader string, userAgent string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, targetUrl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", cookieHeader)

	if strings.Contains(strings.ToLower(userAgent), "firefox") {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	} else {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("sec-ch-ua", `"Not_A Brand";v="24", "Chromium";v="131", "Google Chrome";v="131"`)
		req.Header.Set("sec-ch-ua-mobile", "?0")
		req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	}

	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Referer", "https://infokhs.umm.ac.id/jadwal-kuliah")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Cookie Refresh via Python headless browser
type RefreshResult struct {
	Status      string `json:"status"`
	NIM         string `json:"nim"`
	Name        string `json:"name"`
	CookiesJSON string `json:"cookies_json"`
	UserAgent   string `json:"user_agent"`
	Error       string `json:"error"`
}

// isServerRefreshDisabled bernilai true jika server sengaja tidak boleh login
// sendiri (karena IP Azure diblokir Cloudflare) dan harus menunggu cookie donor.
func isServerRefreshDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("DISABLE_SERVER_REFRESH")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// looksLikeCloudflareBlock mendeteksi pesan error yang berasal dari challenge Cloudflare.
func looksLikeCloudflareBlock(msg string) bool {
	m := strings.ToLower(msg)
	for _, keyword := range []string{"cloudflare", "just a moment", "challenge", "turnstile"} {
		if strings.Contains(m, keyword) {
			return true
		}
	}
	return false
}

// mergeProfileResult menyimpan hasil eksekusi bot ke profil di database.
// Jika cookie di database lebih baru (mis. baru saja di-push laptop donor),
// cookie lama dari hasil eksekusi TIDAK boleh menimpanya.
func mergeProfileResult(target *Profile, result *Profile) {
	if target.CookieUpdatedAt.After(result.CookieUpdatedAt) {
		result.CookiesJSON = target.CookiesJSON
		result.CookieUpdatedAt = target.CookieUpdatedAt
		if target.Name != "" {
			result.Name = target.Name
		}
		result.FailedCount = target.FailedCount
		result.LastRunStatus = target.LastRunStatus
		result.LastRunMsg = target.LastRunMsg
		result.LastRunTime = target.LastRunTime
	}
	*target = *result
}

const failureAlertThreshold = 10

// normalizePhoneNumber mengubah 08xx/8xx menjadi format 62xx yang dipakai Fonnte.
func normalizePhoneNumber(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	number := b.String()
	if number == "" {
		return ""
	}
	if strings.HasPrefix(number, "0") {
		return "62" + number[1:]
	}
	if strings.HasPrefix(number, "8") {
		return "62" + number
	}
	return number
}

// sendFailureAlert mengirim pengingat WA satu kali saat profil otomatis dihentikan.
func sendFailureAlert(nim, name, fallbackToken string) {
	configMutex.RLock()
	number := normalizePhoneNumber(globalConfig.WAAlertNumber)
	token := globalConfig.WAToken
	configMutex.RUnlock()

	if token == "" {
		token = fallbackToken
	}

	if number == "" {
		AddLog("warning", "Pengingat WA tidak dikirim: nomor WA pengingat belum diatur di Pengaturan Global.")
		return
	}
	if token == "" {
		AddLog("warning", "Pengingat WA tidak dikirim: WhatsApp Token (Fonnte) global belum diatur.")
		return
	}
	if name == "" {
		name = "Tanpa Nama"
	}

	loc := time.FixedZone("WIB", 7*60*60)
	waktu := time.Now().In(loc).Format("02/01/2006 15:04 WIB")
	pesan := fmt.Sprintf(
		"⚠️ BOT PRESENSI DIHENTIKAN SEMENTARA\n\n"+
			"Akun: %s (%s)\n"+
			"Bot gagal mengecek presensi %d kali berturut-turut.\n"+
			"Kemungkinan cookie/sesi sudah kedaluwarsa atau diblokir Cloudflare.\n\n"+
			"Bot akun ini berhenti sementara sampai cookie baru diberikan.\n"+
			"Silakan perbarui cookie lewat dashboard atau jalankan cookie donor dari laptop.\n\n"+
			"Waktu: %s",
		name, nim, failureAlertThreshold, waktu,
	)

	AddLog("system", "📨 Mengirim pengingat WA ke %s untuk NIM %s...", number, nim)
	sendWhatsApp(pesan, token, number)
}

// claimFailureAlert menandai pengingat sudah terkirim secara atomik di database,
// sehingga pengingat tidak pernah terkirim dua kali walau ada run bersamaan.
func claimFailureAlert(nim string) bool {
	studentProfilesMutex.Lock()
	defer studentProfilesMutex.Unlock()
	for i := range studentProfiles {
		if studentProfiles[i].NIM == nim {
			if studentProfiles[i].FailureAlertSent {
				return false
			}
			studentProfiles[i].FailureAlertSent = true
			return true
		}
	}
	return false
}

// maybePauseAfterFailures menghentikan sementara profil yang gagal 10 kali
// dan mengirim pengingat WA satu kali. Profil aktif kembali saat cookie baru masuk.
func maybePauseAfterFailures(p *Profile) {
	if p.Paused || p.FailedCount < failureAlertThreshold {
		return
	}

	p.Paused = true
	p.PauseReason = fmt.Sprintf("Dihentikan sementara setelah %d kali gagal mengecek presensi. Menunggu cookie baru.", p.FailedCount)
	p.LastRunStatus = "paused"
	p.LastRunMsg = p.PauseReason

	AddLog("error", "⛔ NIM %s (%s): %s", p.NIM, p.Name, p.PauseReason)

	if p.FailureAlertSent {
		return
	}

	configMutex.RLock()
	hasNumber := normalizePhoneNumber(globalConfig.WAAlertNumber) != ""
	hasToken := strings.TrimSpace(globalConfig.WAToken) != "" || strings.TrimSpace(p.WAToken) != ""
	configMutex.RUnlock()
	if !hasNumber {
		AddLog("warning", "NIM %s: Nomor WA pengingat belum diisi di Pengaturan Global.", p.NIM)
		return
	}
	if !hasToken {
		AddLog("warning", "NIM %s: WhatsApp Token (Fonnte) global belum diisi, pengingat tidak dikirim.", p.NIM)
		return
	}

	if !claimFailureAlert(p.NIM) {
		return
	}
	p.FailureAlertSent = true
	go sendFailureAlert(p.NIM, p.Name, p.WAToken)
}

// markRefreshFailure menentukan status profil saat auto-refresh gagal.
func markRefreshFailure(p *Profile, prefix string, err error) {
	switch {
	case looksLikeCloudflareBlock(err.Error()):
		p.LastRunStatus = "needs_cookie"
		p.LastRunMsg = fmt.Sprintf("%s: Cloudflare memblokir IP server, menunggu cookie baru dari laptop donor (%v)", prefix, err)
	case isServerRefreshDisabled():
		p.LastRunStatus = "needs_cookie"
		p.LastRunMsg = fmt.Sprintf("%s: auto-refresh server dinonaktifkan, menunggu cookie dari laptop donor (%v)", prefix, err)
	default:
		p.LastRunStatus = "expired"
		p.LastRunMsg = fmt.Sprintf("%s: %v", prefix, err)
	}
}

// pushToken mengembalikan token untuk endpoint /api/cookies/push.
func pushToken() string {
	return strings.TrimSpace(os.Getenv("COOKIE_PUSH_TOKEN"))
}

// checkPushToken memvalidasi header X-Push-Token atau Bearer token.
func checkPushToken(r *nethttp.Request) bool {
	expected := pushToken()
	if expected == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Push-Token"))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
			got = strings.TrimSpace(auth[7:])
		}
	}
	if got == "" {
		// Dukungan query string untuk kemudahan pemakaian dari browser/curl.
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	// Perbandingan sederhana; token ini hanya untuk relay cookie milik sendiri.
	return got == expected
}

// normalizeCookiesJSON menerima cookies_json dalam beberapa bentuk:
// string JSON, objek {user_agent, cookies}, atau array cookie.
func normalizeCookiesJSON(raw json.RawMessage, userAgent string) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("cookies_json wajib diisi")
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", fmt.Errorf("cookies_json wajib diisi")
	}

	// Bentuk paling umum: string JSON.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return "", fmt.Errorf("cookies_json kosong")
		}
		if header, _ := parseCookies(asString); header == "" {
			return "", fmt.Errorf("format cookies_json tidak dikenali")
		}
		return asString, nil
	}

	// Objek {user_agent, cookies:[...]}.
	if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			UserAgent string          `json:"user_agent"`
			Cookies   json.RawMessage `json:"cookies"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && len(obj.Cookies) > 0 {
			ua := obj.UserAgent
			if ua == "" {
				ua = userAgent
			}
			if ua == "" {
				ua = USER_AGENT
			}
			wrapped, _ := json.Marshal(map[string]interface{}{
				"user_agent": ua,
				"cookies":    obj.Cookies,
			})
			if header, _ := parseCookies(string(wrapped)); header == "" {
				return "", fmt.Errorf("daftar cookies kosong/tidak valid")
			}
			return string(wrapped), nil
		}
	}

	// Array cookie.
	if strings.HasPrefix(trimmed, "[") {
		var arr []CookieItem
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
			ua := userAgent
			if ua == "" {
				ua = USER_AGENT
			}
			wrapped, _ := json.Marshal(map[string]interface{}{
				"user_agent": ua,
				"cookies":    arr,
			})
			return string(wrapped), nil
		}
	}

	return "", fmt.Errorf("format cookies_json tidak dikenali")
}

func encodePassword(plain string) string {
	return base64.StdEncoding.EncodeToString([]byte(plain))
}

func decodePassword(encoded string) string {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return encoded // fallback: treat as plain text
	}
	return string(data)
}

func refreshCookies(p *Profile) error {
	password := decodePassword(p.Password)
	if p.NIM == "" || password == "" {
		return fmt.Errorf("NIM atau Password kosong, tidak bisa auto-refresh cookie")
	}

	// Mode "donor": server tidak mencoba login sendiri karena IP-nya diblokir Cloudflare.
	// Cookie baru dikirim oleh laptop lewat endpoint /api/cookies/push.
	if isServerRefreshDisabled() {
		return fmt.Errorf("auto-refresh browser di server dinonaktifkan (DISABLE_SERVER_REFRESH=1). Menunggu cookie baru dari laptop donor")
	}

	AddLog("system", "🔄 NIM %s: Memulai auto-refresh cookie via headless browser...", p.NIM)
	p.LastRunStatus = "refreshing"
	p.LastRunMsg = "Sedang refresh cookie via headless browser..."

	// Find python script path
	scriptPath := "cookie_refresher.py"
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		scriptPath = filepath.Join("..", "cookie_refresher.py")
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			return fmt.Errorf("cookie_refresher.py tidak ditemukan")
		}
	}

	// Find python binary
	pythonCmd := "python3"
	if _, err := exec.LookPath("python3"); err != nil {
		pythonCmd = "python"
	}

	cmd := exec.Command(pythonCmd, scriptPath, "--nim", p.NIM, "--password", password)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Timeout: 3 minutes max
	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		// Log stderr (Python logs go here)
		if stderrStr := strings.TrimSpace(stderr.String()); stderrStr != "" {
			for _, line := range strings.Split(stderrStr, "\n") {
				AddLog("info", "🐍 %s", line)
			}
		}

		// Parse JSON result from stdout if available, even on error
		var result RefreshResult
		hasResult := false
		if stdout.Len() > 0 {
			if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr == nil && result.Status != "" {
				hasResult = true
			}
		}

		if err != nil {
			if hasResult && result.Error != "" {
				return fmt.Errorf("cookie_refresher.py gagal: %s", result.Error)
			}
			return fmt.Errorf("cookie_refresher.py gagal: %v", err)
		}

	case <-time.After(3 * time.Minute):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Errorf("cookie refresh timeout (3 menit)")
	}

	// Parse JSON result from stdout
	var result RefreshResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return fmt.Errorf("gagal parse output refresher: %v (raw: %s)", err, stdout.String())
	}

	if result.Status != "success" {
		return fmt.Errorf("refresh gagal: %s", result.Error)
	}

	// Update profile with new cookies
	p.CookiesJSON = result.CookiesJSON
	p.CookieUpdatedAt = time.Now()
	if result.Name != "" && result.Name != "Unknown" {
		p.Name = result.Name
	}
	p.FailedCount = 0
	p.Paused = false
	p.PauseReason = ""
	p.FailureAlertSent = false
	p.LastRunStatus = ""
	p.LastRunMsg = "Cookie berhasil di-refresh otomatis! Bot diaktifkan kembali."

	AddLog("success", "🎉 NIM %s: Cookie berhasil di-refresh! Sesi baru aktif.", p.NIM)

	// Invalidate cached TLS client so it picks up new user-agent
	clientCacheMu.Lock()
	delete(clientCache, p.NIM)
	clientCacheMu.Unlock()

	return nil
}

// Core Bot Executor
func runBotForProfile(p *Profile) []CourseSuccess {
	cookieHeader, userAgent := parseCookies(p.CookiesJSON)
	if cookieHeader == "" {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "expired"
			p.LastRunMsg = "Cookie tidak valid / kosong (Gagal 3 kali)"
		} else {
			p.LastRunMsg = fmt.Sprintf("Cookie tidak valid / kosong (Percobaan %d/3)", p.FailedCount)
		}
		AddLog("error", "NIM %s: Cookie kosong atau salah format (Percobaan %d/3).", p.NIM, p.FailedCount)
		return nil
	}

	var clientProfile profiles.ClientProfile
	if strings.Contains(strings.ToLower(userAgent), "firefox") {
		clientProfile = profiles.Firefox_132
	} else {
		clientProfile = profiles.Chrome_131
	}

	clientCacheMu.Lock()
	cached, exists := clientCache[p.NIM]
	var client tls_client.HttpClient
	var err error
	if exists && cached.UserAgent == userAgent {
		client = cached.Client
		clientCacheMu.Unlock()
	} else {
		clientCacheMu.Unlock()
		options := []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(30),
			tls_client.WithClientProfile(clientProfile),
			tls_client.WithNotFollowRedirects(),
		}
		// Route melalui WARP SOCKS5 proxy jika tersedia
		proxyURL := os.Getenv("PROXY_URL")
		if proxyURL != "" {
			options = append(options, tls_client.WithProxyUrl(proxyURL))
		}
		client, err = tls_client.NewHttpClient(tls_client.NewLogger(), options...)
		if err == nil {
			clientCacheMu.Lock()
			clientCache[p.NIM] = CachedClient{
				Client:    client,
				UserAgent: userAgent,
			}
			clientCacheMu.Unlock()
		}
	}

	if err != nil {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "failed"
			p.LastRunMsg = fmt.Sprintf("Gagal init TLS Client (Gagal 3 kali): %v", err)
		} else {
			p.LastRunMsg = fmt.Sprintf("Gagal init TLS Client (Percobaan %d/3): %v", p.FailedCount, err)
		}
		AddLog("error", "NIM %s: Gagal inisialisasi client (Percobaan %d/3): %v", p.NIM, p.FailedCount, err)
		return nil
	}

	AddLog("info", "🚀 NIM %s (%s): Memeriksa jadwal kuliah...", p.NIM, p.Name)
	resp, err := makeRequest(client, URL_JADWAL, cookieHeader, userAgent)
	if err != nil {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "failed"
			p.LastRunMsg = fmt.Sprintf("Gagal request jadwal (Gagal 3 kali): %v", err)
		} else {
			p.LastRunMsg = fmt.Sprintf("Gagal request jadwal (Percobaan %d/3): %v", p.FailedCount, err)
		}
		AddLog("error", "NIM %s: Gagal memuat jadwal (Percobaan %d/3): %v", p.NIM, p.FailedCount, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "expired"
			p.LastRunMsg = "Cloudflare memblokir request (HTTP 403) (Gagal 3 kali)"
			AddLog("error", "NIM %s: Cloudflare memblokir akses (HTTP 403) (Gagal 3 kali).", p.NIM)

			// Auto-refresh cookies if password is available
			if p.Password != "" {
				AddLog("system", "NIM %s: Mencoba auto-refresh cookie...", p.NIM)
				if err := refreshCookies(p); err != nil {
					AddLog("error", "NIM %s: Auto-refresh gagal: %v", p.NIM, err)
					markRefreshFailure(p, "Cookie expired & auto-refresh gagal", err)
				} else {
					// Retry once with new cookies
					AddLog("info", "NIM %s: Mencoba ulang presensi dengan cookie baru...", p.NIM)
					return runBotForProfile(p)
				}
			} else {
				AddLog("warning", "NIM %s: Password tidak tersedia, tidak bisa auto-refresh. Silakan update cookie manual.", p.NIM)
			}
		} else {
			p.LastRunMsg = fmt.Sprintf("Cloudflare memblokir request (HTTP 403) (Percobaan %d/3)", p.FailedCount)
			AddLog("error", "NIM %s: Cloudflare memblokir akses (HTTP 403) (Percobaan %d/3).", p.NIM, p.FailedCount)
		}
		return nil
	}

	if resp.StatusCode == 301 || resp.StatusCode == 302 {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "expired"
			p.LastRunMsg = "Sesi kedaluwarsa (Redirect ke Login) (Gagal 3 kali)"
			AddLog("warning", "NIM %s: Sesi kedaluwarsa (Gagal 3 kali).", p.NIM)

			// Auto-refresh cookies if password is available
			if p.Password != "" {
				AddLog("system", "NIM %s: Mencoba auto-refresh cookie...", p.NIM)
				if err := refreshCookies(p); err != nil {
					AddLog("error", "NIM %s: Auto-refresh gagal: %v", p.NIM, err)
					markRefreshFailure(p, "Sesi expired & auto-refresh gagal", err)
				} else {
					// Retry once with new cookies
					AddLog("info", "NIM %s: Mencoba ulang presensi dengan cookie baru...", p.NIM)
					return runBotForProfile(p)
				}
			} else {
				AddLog("warning", "NIM %s: Password tidak tersedia, tidak bisa auto-refresh. Silakan update cookie manual.", p.NIM)
			}
		} else {
			p.LastRunMsg = fmt.Sprintf("Sesi kedaluwarsa (Redirect ke Login) (Percobaan %d/3)", p.FailedCount)
			AddLog("warning", "NIM %s: Sesi kedaluwarsa (Percobaan %d/3).", p.NIM, p.FailedCount)
		}
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "failed"
			p.LastRunMsg = fmt.Sprintf("Gagal parse HTML (Gagal 3 kali): %v", err)
		} else {
			p.LastRunMsg = fmt.Sprintf("Gagal parse HTML (Percobaan %d/3): %v", p.FailedCount, err)
		}
		AddLog("error", "NIM %s: Gagal parsing HTML jadwal (Percobaan %d/3): %v", p.NIM, p.FailedCount, err)
		return nil
	}

	// Reset hitungan gagal karena sesi cookie valid dan pengambilan data jadwal sukses
	p.FailedCount = 0

	var presenceLinks []string
	doc.Find("a.btn-success, a.btn-xs").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if exists && (strings.Contains(href, "presensi") || strings.Contains(href, "jadwal-kuliah/presensi")) {
			if strings.HasPrefix(href, "/") {
				href = "https://infokhs.umm.ac.id" + href
			} else if !strings.HasPrefix(href, "http") {
				href = "https://infokhs.umm.ac.id/" + href
			}

			// Deduplicate
			found := false
			for _, l := range presenceLinks {
				if l == href {
					found = true
					break
				}
			}
			if !found {
				presenceLinks = append(presenceLinks, href)
			}
		}
	})

	if len(presenceLinks) == 0 {
		p.LastRunStatus = "no_link"
		p.LastRunTime = time.Now()
		p.LastRunMsg = "Tidak ada tautan presensi matkul aktif."
		AddLog("info", "ℹ️ NIM %s: Selesai memeriksa. Tidak ada jadwal mata kuliah aktif yang butuh presensi.", p.NIM)
		return nil
	}

	totalSuccess := 0
	var matkulBerhasil []CourseSuccess
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, link := range presenceLinks {
		wg.Add(1)
		go func(idx int, targetLink string) {
			defer wg.Done()

			pResp, pErr := makeRequest(client, targetLink, cookieHeader, userAgent)
			if pErr != nil || pResp.StatusCode != 200 {
				AddLog("error", "NIM %s: [%d] Gagal memuat halaman detail presensi.", p.NIM, idx+1)
				if pResp != nil {
					pResp.Body.Close()
				}
				return
			}

			pDoc, err := goquery.NewDocumentFromReader(pResp.Body)
			pResp.Body.Close()
			if err != nil {
				AddLog("error", "NIM %s: [%d] Gagal parsing detail presensi.", p.NIM, idx+1)
				return
			}

			// Scrape Course Name
			courseName := "Tidak diketahui"
			pDoc.Find("b, strong").Each(func(_ int, s *goquery.Selection) {
				text := strings.ToLower(strings.TrimSpace(s.Text()))
				if strings.Contains(text, "mata kuliah") {
					parent := s.Parent()
					if parent.HasClass("col-sm-3") {
						nextDiv := parent.Next()
						if nextDiv.HasClass("col-sm-9") {
							val := strings.TrimSpace(nextDiv.Text())
							if val != "" {
								courseName = val
							}
						}
					}
				}
			})

			// Parse Table Headers
			var headers []string
			hadirIdx, aksiIdx, mulaiIdx, selesaiIdx := -1, -1, -1, -1

			pDoc.Find("table thead th").Each(func(hIdx int, s *goquery.Selection) {
				hText := strings.ToLower(strings.TrimSpace(s.Text()))
				headers = append(headers, hText)
				if strings.Contains(hText, "hadir") {
					hadirIdx = hIdx
				}
				if strings.Contains(hText, "aksi") {
					aksiIdx = hIdx
				}
				if strings.Contains(hText, "mulai") {
					mulaiIdx = hIdx
				}
				if strings.Contains(hText, "selesai") {
					selesaiIdx = hIdx
				}
			})

			if hadirIdx == -1 {
				hadirIdx = 5
			}
			if aksiIdx == -1 {
				aksiIdx = 6
			}
			if mulaiIdx == -1 {
				mulaiIdx = 3
			}
			if selesaiIdx == -1 {
				selesaiIdx = 4
			}

			alreadyVCount := 0
			var buttonsToClick []map[string]string

			// Locate active presence buttons
			pDoc.Find("table tbody tr").Each(func(rIdx int, s *goquery.Selection) {
				tds := s.Find("td")
				if tds.Length() <= hadirIdx || tds.Length() <= aksiIdx {
					return
				}

				hadirText := strings.ToLower(strings.TrimSpace(tds.Eq(hadirIdx).Text()))
				aksiLink := ""

				tds.Eq(aksiIdx).Find("a").Each(func(_ int, a *goquery.Selection) {
					href, exists := a.Attr("href")
					if exists && strings.Contains(href, "/proses/") {
						if strings.HasPrefix(href, "/") {
							href = "https://infokhs.umm.ac.id" + href
						} else if !strings.HasPrefix(href, "http") {
							href = "https://infokhs.umm.ac.id/" + href
						}
						aksiLink = href
					}
				})

				mulaiText := ""
				if tds.Length() > mulaiIdx && mulaiIdx != -1 {
					mulaiText = strings.TrimSpace(tds.Eq(mulaiIdx).Text())
				}
				selesaiText := ""
				if tds.Length() > selesaiIdx && selesaiIdx != -1 {
					selesaiText = strings.TrimSpace(tds.Eq(selesaiIdx).Text())
				}

				hasV := strings.Contains(hadirText, "v")
				hasButton := aksiLink != ""

				if hasButton {
					if hasV {
						alreadyVCount++
					} else {
						buttonsToClick = append(buttonsToClick, map[string]string{
							"rIdx":    fmt.Sprintf("%d", rIdx+1),
							"link":    aksiLink,
							"mulai":   mulaiText,
							"selesai": selesaiText,
						})
					}
				}
			})

			if len(buttonsToClick) == 0 {
				return
			}

			clickCount := 0
			var successes []CourseSuccess

			for _, btn := range buttonsToClick {
				aResp, aErr := makeRequest(client, btn["link"], cookieHeader, userAgent)
				if aErr != nil {
					AddLog("error", "NIM %s: [%d] Gagal klik link presensi: %v", p.NIM, idx+1, aErr)
					continue
				}
				if aResp.StatusCode == 301 || aResp.StatusCode == 302 {
					AddLog("warning", "NIM %s: [%d] Sesi terputus saat klik.", p.NIM, idx+1)
					aResp.Body.Close()
					return
				}
				aResp.Body.Close()

				clickCount++

				timeInfo := "Tidak diketahui"
				if btn["mulai"] != "" && btn["selesai"] != "" {
					timeInfo = fmt.Sprintf("%s - %s", btn["mulai"], btn["selesai"])
				}
				successes = append(successes, CourseSuccess{CourseName: courseName, TimeInfo: timeInfo})
			}

			if clickCount > 0 {
				mu.Lock()
				totalSuccess += clickCount
				matkulBerhasil = append(matkulBerhasil, successes...)
				mu.Unlock()
			}
		}(i, link)
	}

	wg.Wait()

	p.LastRunTime = time.Now()
	if totalSuccess > 0 {
		p.LastRunStatus = "success"
		var lines []string
		for _, cs := range matkulBerhasil {
			lines = append(lines, fmt.Sprintf("• %s (%s)", cs.CourseName, cs.TimeInfo))
		}
		p.LastRunMsg = fmt.Sprintf("Sukses klik %d tombol: %s", totalSuccess, strings.Join(lines, ", "))
		AddLog("success", "✅ NIM %s: Berhasil melakukan %d presensi otomatis pada:\n%s", p.NIM, totalSuccess, strings.Join(lines, "\n"))
	} else {
		p.LastRunStatus = "no_link"
		p.LastRunMsg = "Jadwal terbuka tapi tidak ada tombol presensi baru"
		AddLog("info", "ℹ️ NIM %s: Selesai memeriksa %d mata kuliah. Tidak ada presensi baru yang aktif.", p.NIM, len(presenceLinks))
	}

	return matkulBerhasil
}

// Background scheduler
func runBotForAllActive() {
	studentProfilesMutex.RLock()
	var activeProfiles []Profile
	pausedCount := 0
	for _, p := range studentProfiles {
		if !p.IsActive {
			continue
		}
		if p.Paused {
			pausedCount++
			continue
		}
		activeProfiles = append(activeProfiles, p)
	}
	studentProfilesMutex.RUnlock()

	if pausedCount > 0 {
		AddLog("info", "⏸️ %d akun dilewati karena dihentikan sementara (menunggu cookie baru).", pausedCount)
	}

	if len(activeProfiles) == 0 {
		AddLog("info", "Tidak ada akun aktif dalam scheduler.")
		return
	}

	var allSuccesses []SuccessRecord
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Run concurrently using Goroutines
	for i := range activeProfiles {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			p := &activeProfiles[idx]
			successes := runBotForProfile(p)
			maybePauseAfterFailures(p)

			// Resolve WA configurations
			waToken := p.WAToken
			if waToken == "" {
				configMutex.RLock()
				waToken = globalConfig.WAToken
				configMutex.RUnlock()
			}
			waGroup := p.WAGroup
			if waGroup == "" {
				configMutex.RLock()
				waGroup = globalConfig.WAGroup
				configMutex.RUnlock()
			}

			// Safe append to successes slice
			mu.Lock()
			for _, cs := range successes {
				allSuccesses = append(allSuccesses, SuccessRecord{
					NIM:        p.NIM,
					Name:       p.Name,
					CourseName: cs.CourseName,
					TimeInfo:   cs.TimeInfo,
					WAToken:    waToken,
					WAGroup:    waGroup,
				})
			}
			mu.Unlock()

			// Update state in db
			studentProfilesMutex.Lock()
			for j := range studentProfiles {
				if studentProfiles[j].NIM == p.NIM {
					mergeProfileResult(&studentProfiles[j], p)
					break
				}
			}
			studentProfilesMutex.Unlock()
		}(i)
	}

	wg.Wait()
	saveStudentProfiles()

	// Aggregate and send WhatsApp notifications
	if len(allSuccesses) > 0 {
		// Group by target (WAToken + "::" + WAGroup)
		targetGroups := make(map[string][]SuccessRecord)
		for _, rec := range allSuccesses {
			key := rec.WAToken + "::" + rec.WAGroup
			targetGroups[key] = append(targetGroups[key], rec)
		}

		for key, records := range targetGroups {
			parts := strings.SplitN(key, "::", 2)
			token := parts[0]
			group := parts[1]

			if token == "" || group == "" {
				continue
			}

			// Group by class session (CourseName + "::" + TimeInfo)
			classGroups := make(map[string][]SuccessRecord)
			for _, rec := range records {
				classKey := rec.CourseName + "::" + rec.TimeInfo
				classGroups[classKey] = append(classGroups[classKey], rec)
			}

			// Construct and send message for each class session
			for classKey, classRecs := range classGroups {
				classParts := strings.SplitN(classKey, "::", 2)
				courseName := classParts[0]
				timeInfo := classParts[1]

				if len(classRecs) > 1 {
					// Multiple students in the same class session -> Combined message!
					var studentList []string
					for _, r := range classRecs {
						studentList = append(studentList, fmt.Sprintf("• %s (%s)", r.Name, r.NIM))
					}
					pesan := fmt.Sprintf("🔔 PRESENSI OTOMATIS - KELAS BERSAMA\n\n📚 Matkul: %s\n🕒 Waktu: %s\n\nDaftar Mahasiswa Berhasil:\n%s",
						courseName, timeInfo, strings.Join(studentList, "\n"))
					sendWhatsApp(pesan, token, group)
				} else {
					// Only 1 student in this class session -> Single-student message!
					r := classRecs[0]
					pesan := fmt.Sprintf("Presensi Otomatis - NIM %s (%s)\n\n• %s (%s)",
						r.NIM, r.Name, courseName, timeInfo)
					sendWhatsApp(pesan, token, group)
				}
			}
		}
	}
}

// Ping Keep-Alive agar sesi PHP cookie tetap aktif dan tidak kedaluwarsa
func pingKeepAliveForProfile(p *Profile) {
	if p.Paused {
		return
	}
	cookieHeader, userAgent := parseCookies(p.CookiesJSON)
	if cookieHeader == "" {
		return
	}

	var clientProfile profiles.ClientProfile
	if strings.Contains(strings.ToLower(userAgent), "firefox") {
		clientProfile = profiles.Firefox_132
	} else {
		clientProfile = profiles.Chrome_131
	}

	clientCacheMu.Lock()
	cached, exists := clientCache[p.NIM]
	var client tls_client.HttpClient
	var err error
	if exists && cached.UserAgent == userAgent {
		client = cached.Client
		clientCacheMu.Unlock()
	} else {
		clientCacheMu.Unlock()
		options := []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(30),
			tls_client.WithClientProfile(clientProfile),
			tls_client.WithNotFollowRedirects(),
		}
		proxyURL := os.Getenv("PROXY_URL")
		if proxyURL != "" {
			options = append(options, tls_client.WithProxyUrl(proxyURL))
		}
		client, err = tls_client.NewHttpClient(tls_client.NewLogger(), options...)
		if err == nil {
			clientCacheMu.Lock()
			clientCache[p.NIM] = CachedClient{
				Client:    client,
				UserAgent: userAgent,
			}
			clientCacheMu.Unlock()
		}
	}

	if err != nil {
		AddLog("warning", "⚠️ NIM %s: Gagal inisialisasi client untuk keep-alive: %v", p.NIM, err)
		return
	}

	resp, err := makeRequest(client, URL_JADWAL, cookieHeader, userAgent)
	if err != nil {
		AddLog("warning", "⚠️ NIM %s: Ping keep-alive gagal request: %v", p.NIM, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		AddLog("info", "💓 NIM %s (%s): Ping keep-alive sesi berhasil (HTTP 200, sesi aktif diperpanjang).", p.NIM, p.Name)
	} else if resp.StatusCode == 301 || resp.StatusCode == 302 {
		AddLog("warning", "⚠️ NIM %s: Sesi kedaluwarsa saat keep-alive (HTTP %d, redirect ke login).", p.NIM, resp.StatusCode)
	} else if resp.StatusCode == 403 {
		AddLog("warning", "⚠️ NIM %s: Cloudflare 403 saat ping keep-alive.", p.NIM)
	} else {
		AddLog("info", "ℹ️ NIM %s: Ping keep-alive selesai dengan respon HTTP %d.", p.NIM, resp.StatusCode)
	}
}

func pingKeepAliveForAllActive() {
	studentProfilesMutex.RLock()
	profilesCopy := make([]Profile, len(studentProfiles))
	copy(profilesCopy, studentProfiles)
	studentProfilesMutex.RUnlock()

	var wg sync.WaitGroup
	for i := range profilesCopy {
		if !profilesCopy[i].IsActive || profilesCopy[i].Paused {
			continue
		}
		wg.Add(1)
		go func(p Profile) {
			defer wg.Done()
			pingKeepAliveForProfile(&p)
		}(profilesCopy[i])
	}
	wg.Wait()
}

func startScheduler() {
	go func() {
		// Small delay on start
		time.Sleep(5 * time.Second)
		for {
			configMutex.RLock()
			interval := globalConfig.CheckInterval
			startHour := globalConfig.StartHour
			endHour := globalConfig.EndHour
			configMutex.RUnlock()

			if interval <= 0 {
				interval = 5
			}

			loc := time.FixedZone("WIB", 7*60*60)
			now := time.Now().In(loc)
			hour := now.Hour()

			if hour >= startHour && hour < endHour {
				AddLog("system", "Memulai siklus pemeriksaan presensi terjadwal...")
				runBotForAllActive()
				AddLog("system", "Siklus selesai. Menunggu %d detik...", interval)
				time.Sleep(time.Duration(interval) * time.Second)
			} else {
				AddLog("system", "Pukul %s WIB: Di luar jam operasional (%02d:00 - %02d:00). Menjalankan ping keep-alive sesi...",
					now.Format("15:04:05"), startHour, endHour)
				pingKeepAliveForAllActive()
				AddLog("system", "Ping keep-alive selesai. Menunggu 15 menit untuk ping berikutnya...")
				time.Sleep(15 * time.Minute)
			}
		}
	}()
}

func basicAuthMiddleware(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		expectedPassword := os.Getenv("WEB_PASSWORD")
		if expectedPassword == "" {
			next.ServeHTTP(w, r)
			return
		}

		username, password, ok := r.BasicAuth()
		if !ok || username != "admin" || password != expectedPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="UMM Bot Presensi"`)
			w.WriteHeader(nethttp.StatusUnauthorized)
			w.Write([]byte("Unauthorized. Please enter username 'admin' and your custom WEB_PASSWORD.\n"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// REST HTTP Server
func main() {
	// Load Env if any (from root or local folder)
	godotenv.Load("../.env")
	godotenv.Load(".env")

	AddLog("system", "Inisialisasi sistem basis data bot...")
	loadData()

	if pushToken() != "" {
		AddLog("system", "Mode donor aktif: endpoint /api/cookies/push siap menerima cookie dari laptop.")
	}
	if isServerRefreshDisabled() {
		AddLog("system", "Auto-refresh browser di server DINONAKTIFKAN (DISABLE_SERVER_REFRESH=1). Server menunggu cookie dari laptop donor.")
	}

	AddLog("system", "Memulai scheduler pengecekan latar belakang...")
	startScheduler()

	// API: Get/Post/Delete Profiles
	nethttp.Handle("/api/profiles", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == nethttp.MethodOptions {
			w.WriteHeader(nethttp.StatusOK)
			return
		}

		if r.Method == nethttp.MethodGet {
			studentProfilesMutex.RLock()
			defer studentProfilesMutex.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(studentProfiles)
			return
		}

		if r.Method == nethttp.MethodPost {
			var newP Profile
			if err := json.NewDecoder(r.Body).Decode(&newP); err != nil {
				nethttp.Error(w, err.Error(), nethttp.StatusBadRequest)
				return
			}

			newP.NIM = strings.TrimSpace(newP.NIM)
			newP.Name = strings.TrimSpace(newP.Name)
			if newP.NIM == "" || newP.Name == "" {
				nethttp.Error(w, "NIM dan Nama wajib diisi!", nethttp.StatusBadRequest)
				return
			}

			// Encode password with base64 if provided as plain text
			if newP.Password != "" {
				// Check if it's already base64
				if _, err := base64.StdEncoding.DecodeString(newP.Password); err != nil {
					newP.Password = encodePassword(newP.Password)
				}
			}

			studentProfilesMutex.Lock()
			defer studentProfilesMutex.Unlock()

			found := false
			for i, p := range studentProfiles {
				if p.NIM == newP.NIM {
					// Update profile, keep previous logs intact
					newP.LastRunStatus = p.LastRunStatus
					newP.LastRunTime = p.LastRunTime
					newP.LastRunMsg = p.LastRunMsg
					// Keep existing password if not provided
					if newP.Password == "" {
						newP.Password = p.Password
					}

					// Pertahankan status penghentian & pengingat dari data lama.
					newP.Paused = p.Paused
					newP.PauseReason = p.PauseReason
					newP.FailureAlertSent = p.FailureAlertSent
					newP.FailedCount = p.FailedCount

					incomingCookies := strings.TrimSpace(newP.CookiesJSON)
					oldCookies := strings.TrimSpace(p.CookiesJSON)
					switch {
					case incomingCookies == "":
						// Cookie tidak dikirim: pakai cookie lama.
						newP.CookiesJSON = p.CookiesJSON
						newP.CookieUpdatedAt = p.CookieUpdatedAt
					case incomingCookies == oldCookies:
						// Cookie sama (mis. hanya edit nama): jangan reset pause.
						newP.CookieUpdatedAt = p.CookieUpdatedAt
					default:
						// Cookie benar-benar berubah → aktifkan kembali bot.
						newP.CookieUpdatedAt = time.Now()
						newP.FailedCount = 0
						newP.Paused = false
						newP.PauseReason = ""
						newP.FailureAlertSent = false
						newP.LastRunStatus = ""
						newP.LastRunMsg = "Cookie baru disimpan lewat dashboard. Bot diaktifkan kembali."
					}
					studentProfiles[i] = newP
					found = true
					break
				}
			}
			if !found {
				if strings.TrimSpace(newP.CookiesJSON) != "" {
					newP.CookieUpdatedAt = time.Now()
				}
				studentProfiles = append(studentProfiles, newP)
			}
			saveStudentProfiles()

			AddLog("system", "Profil NIM %s (%s) disimpan/diupdate.", newP.NIM, newP.Name)
			w.WriteHeader(nethttp.StatusOK)
			w.Write([]byte("success"))
			return
		}

		if r.Method == nethttp.MethodDelete {
			nim := r.URL.Query().Get("nim")
			if nim == "" {
				nethttp.Error(w, "NIM parameter is required", nethttp.StatusBadRequest)
				return
			}

			studentProfilesMutex.Lock()
			defer studentProfilesMutex.Unlock()

			for i, p := range studentProfiles {
				if p.NIM == nim {
					studentProfiles = append(studentProfiles[:i], studentProfiles[i+1:]...)
					saveStudentProfiles()
					AddLog("system", "Profil NIM %s dihapus.", nim)
					w.WriteHeader(nethttp.StatusOK)
					w.Write([]byte("success"))
					return
				}
			}

			nethttp.Error(w, "Profil tidak ditemukan", nethttp.StatusNotFound)
			return
		}

		nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
	})))

	// API: Global Config
	nethttp.Handle("/api/config", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == nethttp.MethodOptions {
			w.WriteHeader(nethttp.StatusOK)
			return
		}

		if r.Method == nethttp.MethodGet {
			configMutex.RLock()
			defer configMutex.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(globalConfig)
			return
		}

		if r.Method == nethttp.MethodPost {
			var newCfg GlobalConfig
			if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
				nethttp.Error(w, err.Error(), nethttp.StatusBadRequest)
				return
			}

			configMutex.Lock()
			globalConfig = newCfg
			saveConfig()
			configMutex.Unlock()

			AddLog("system", "Konfigurasi global diperbarui.")
			w.WriteHeader(nethttp.StatusOK)
			w.Write([]byte("success"))
			return
		}

		nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
	})))

	// API: Run Bot Immediately
	nethttp.Handle("/api/run-now", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == nethttp.MethodOptions {
			w.WriteHeader(nethttp.StatusOK)
			return
		}

		if r.Method != nethttp.MethodPost {
			nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			NIM string `json:"nim"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		go func() {
			if payload.NIM == "" {
				AddLog("system", "Menjalankan paksa bot presensi untuk semua akun aktif...")
				runBotForAllActive()
			} else {
				studentProfilesMutex.Lock()
				var targetIdx = -1
				for i, p := range studentProfiles {
					if p.NIM == payload.NIM {
						targetIdx = i
						break
					}
				}
				studentProfilesMutex.Unlock()

				if targetIdx != -1 {
					studentProfilesMutex.Lock()
					pCopy := studentProfiles[targetIdx]
					studentProfilesMutex.Unlock()

					if pCopy.Paused {
						AddLog("warning", "⏸️ NIM %s: Dilewati karena dihentikan sementara. Berikan cookie baru untuk mengaktifkan kembali.", pCopy.NIM)
						return
					}

					AddLog("system", "Menjalankan paksa bot presensi NIM %s...", pCopy.NIM)
					successes := runBotForProfile(&pCopy)
					maybePauseAfterFailures(&pCopy)

					// Send WA immediately for manual runs
					if len(successes) > 0 {
						waToken := pCopy.WAToken
						if waToken == "" {
							configMutex.RLock()
							waToken = globalConfig.WAToken
							configMutex.RUnlock()
						}
						waGroup := pCopy.WAGroup
						if waGroup == "" {
							configMutex.RLock()
							waGroup = globalConfig.WAGroup
							configMutex.RUnlock()
						}

						if waToken != "" && waGroup != "" {
							var lines []string
							for _, cs := range successes {
								lines = append(lines, fmt.Sprintf("• %s (%s)", cs.CourseName, cs.TimeInfo))
							}
							pesan := fmt.Sprintf("Presensi Otomatis - NIM %s (%s)\n\n%s", pCopy.NIM, pCopy.Name, strings.Join(lines, "\n"))
							sendWhatsApp(pesan, waToken, waGroup)
						}
					}

					studentProfilesMutex.Lock()
					for i := range studentProfiles {
						if studentProfiles[i].NIM == pCopy.NIM {
							mergeProfileResult(&studentProfiles[i], &pCopy)
							break
						}
					}
					studentProfilesMutex.Unlock()
					saveStudentProfiles()
				} else {
					AddLog("error", "Pemicu manual gagal: NIM %s tidak ditemukan.", payload.NIM)
				}
			}
		}()

		w.WriteHeader(nethttp.StatusOK)
		w.Write([]byte("triggered"))
	})))

	// API: Refresh Cookies via headless browser
	nethttp.Handle("/api/refresh-cookies", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == nethttp.MethodOptions {
			w.WriteHeader(nethttp.StatusOK)
			return
		}

		if r.Method != nethttp.MethodPost {
			nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			NIM string `json:"nim"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if payload.NIM == "" {
			nethttp.Error(w, "NIM wajib diisi", nethttp.StatusBadRequest)
			return
		}

		go func() {
			studentProfilesMutex.Lock()
			var targetIdx = -1
			for i, p := range studentProfiles {
				if p.NIM == payload.NIM {
					targetIdx = i
					break
				}
			}
			if targetIdx == -1 {
				studentProfilesMutex.Unlock()
				AddLog("error", "Refresh cookie gagal: NIM %s tidak ditemukan.", payload.NIM)
				return
			}

			pCopy := studentProfiles[targetIdx]
			studentProfilesMutex.Unlock()

			if pCopy.Password == "" {
				AddLog("error", "NIM %s: Password tidak tersedia. Silakan tambahkan password di pengaturan profil untuk menggunakan fitur auto-refresh.", pCopy.NIM)
				return
			}

			if err := refreshCookies(&pCopy); err != nil {
				AddLog("error", "NIM %s: Refresh cookie gagal: %v", pCopy.NIM, err)
				markRefreshFailure(&pCopy, "Refresh cookie manual gagal", err)
			}

			studentProfilesMutex.Lock()
			for i := range studentProfiles {
				if studentProfiles[i].NIM == pCopy.NIM {
					mergeProfileResult(&studentProfiles[i], &pCopy)
					break
				}
			}
			studentProfilesMutex.Unlock()
			saveStudentProfiles()
		}()

		w.WriteHeader(nethttp.StatusOK)
		w.Write([]byte("refresh_triggered"))
	})))

	// API: Push cookie dari laptop (donor) ke server
	// Dipakai ketika IP server (Azure) diblokir Cloudflare, sehingga login
	// dilakukan di laptop yang IP-nya bersih lalu cookie dikirim ke sini.
	nethttp.Handle("/api/cookies/push", nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Push-Token, Authorization")

		if r.Method == nethttp.MethodOptions {
			w.WriteHeader(nethttp.StatusOK)
			return
		}
		if r.Method != nethttp.MethodPost {
			nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
			return
		}
		if pushToken() == "" {
			nethttp.Error(w, "Endpoint push belum aktif. Set env COOKIE_PUSH_TOKEN terlebih dahulu.", nethttp.StatusServiceUnavailable)
			return
		}
		if !checkPushToken(r) {
			nethttp.Error(w, "Token push salah atau tidak dikirim.", nethttp.StatusUnauthorized)
			return
		}

		var payload struct {
			NIM         string          `json:"nim"`
			CookiesJSON json.RawMessage `json:"cookies_json"`
			UserAgent   string          `json:"user_agent"`
			Name        string          `json:"name"`
			Source      string          `json:"source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			nethttp.Error(w, "Body JSON tidak valid: "+err.Error(), nethttp.StatusBadRequest)
			return
		}

		nim := strings.TrimSpace(payload.NIM)
		if nim == "" {
			nethttp.Error(w, "NIM wajib diisi", nethttp.StatusBadRequest)
			return
		}

		cookiesJSON, err := normalizeCookiesJSON(payload.CookiesJSON, strings.TrimSpace(payload.UserAgent))
		if err != nil {
			nethttp.Error(w, err.Error(), nethttp.StatusBadRequest)
			return
		}

		source := strings.TrimSpace(payload.Source)
		if source == "" {
			source = "laptop-donor"
		}

		studentProfilesMutex.Lock()
		idx := -1
		for i := range studentProfiles {
			if strings.EqualFold(studentProfiles[i].NIM, nim) {
				idx = i
				break
			}
		}
		if idx == -1 {
			studentProfilesMutex.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(nethttp.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "NIM " + nim + " belum terdaftar di dashboard server"})
			return
		}

		p := &studentProfiles[idx]
		p.CookiesJSON = cookiesJSON
		if name := strings.TrimSpace(payload.Name); name != "" && name != "Unknown" {
			p.Name = name
		}
		p.FailedCount = 0
		p.Paused = false
		p.PauseReason = ""
		p.FailureAlertSent = false
		p.LastRunStatus = ""
		p.LastRunMsg = fmt.Sprintf("Cookie baru diterima dari %s. Bot diaktifkan kembali.", source)
		p.CookieUpdatedAt = time.Now()
		savedNIM := p.NIM
		savedName := p.Name
		saveStudentProfiles()
		studentProfilesMutex.Unlock()

		// Buang cache TLS client lama agar user-agent/cookie baru langsung dipakai.
		clientCacheMu.Lock()
		delete(clientCache, savedNIM)
		clientCacheMu.Unlock()

		AddLog("success", "🍪 NIM %s (%s): Cookie baru diterima dari %s.", savedNIM, savedName, source)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success", "nim": savedNIM})
	}))

	// API: Daftar profil yang butuh cookie baru (dipoll oleh laptop donor)
	nethttp.Handle("/api/cookies/needs-refresh", nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Push-Token, Authorization")

		if r.Method == nethttp.MethodOptions {
			w.WriteHeader(nethttp.StatusOK)
			return
		}
		if r.Method != nethttp.MethodGet {
			nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
			return
		}
		if pushToken() == "" {
			nethttp.Error(w, "Endpoint push belum aktif. Set env COOKIE_PUSH_TOKEN terlebih dahulu.", nethttp.StatusServiceUnavailable)
			return
		}
		if !checkPushToken(r) {
			nethttp.Error(w, "Token push salah atau tidak dikirim.", nethttp.StatusUnauthorized)
			return
		}

		includeAll := strings.EqualFold(r.URL.Query().Get("all"), "1") || strings.EqualFold(r.URL.Query().Get("all"), "true")

		type NeedItem struct {
			NIM             string    `json:"nim"`
			Name            string    `json:"name"`
			Status          string    `json:"last_run_status"`
			Message         string    `json:"last_run_msg"`
			FailedCount     int       `json:"failed_count"`
			LastRunTime     time.Time `json:"last_run_time"`
			CookieUpdatedAt time.Time `json:"cookie_updated_at"`
			CookieAgeHours  float64   `json:"cookie_age_hours"`
		}

		// Refresh proaktif sebelum cookie benar-benar mati.
		// Bisa diatur lewat env COOKIE_MAX_AGE_HOURS (0 = nonaktifkan).
		maxAgeHours := 6.0
		if raw := strings.TrimSpace(os.Getenv("COOKIE_MAX_AGE_HOURS")); raw != "" {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				maxAgeHours = v
			}
		}

		studentProfilesMutex.RLock()
		items := make([]NeedItem, 0)
		nims := make([]string, 0)
		for _, p := range studentProfiles {
			if !p.IsActive || p.Password == "" {
				continue
			}
			cookieAge := 0.0
			if !p.CookieUpdatedAt.IsZero() {
				cookieAge = time.Since(p.CookieUpdatedAt).Hours()
			}
			stale := maxAgeHours > 0 && (p.CookieUpdatedAt.IsZero() || cookieAge > maxAgeHours)

			needs := includeAll || stale || p.Paused ||
				p.LastRunStatus == "expired" ||
				p.LastRunStatus == "failed" ||
				p.LastRunStatus == "needs_cookie" ||
				p.LastRunStatus == "paused" ||
				p.FailedCount > 0
			if needs {
				items = append(items, NeedItem{
					NIM:             p.NIM,
					Name:            p.Name,
					Status:          p.LastRunStatus,
					Message:         p.LastRunMsg,
					FailedCount:     p.FailedCount,
					LastRunTime:     p.LastRunTime,
					CookieUpdatedAt: p.CookieUpdatedAt,
					CookieAgeHours:  math.Round(cookieAge*10) / 10,
				})
				nims = append(nims, p.NIM)
			}
		}
		studentProfilesMutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"nims":     nims,
			"profiles": items,
		})
	}))

	// API: Export backup
	nethttp.Handle("/api/export", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		type BackupData struct {
			Profiles []Profile    `json:"profiles"`
			Config   GlobalConfig `json:"config"`
		}

		studentProfilesMutex.RLock()
		configMutex.RLock()
		backup := BackupData{
			Profiles: studentProfiles,
			Config:   globalConfig,
		}
		configMutex.RUnlock()
		studentProfilesMutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=umm_bot_backup.json")
		json.NewEncoder(w).Encode(backup)
	})))

	// API: Import backup
	nethttp.Handle("/api/import", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.Method != nethttp.MethodPost {
			nethttp.Error(w, "Method not allowed", nethttp.StatusMethodNotAllowed)
			return
		}

		type BackupData struct {
			Profiles []Profile    `json:"profiles"`
			Config   GlobalConfig `json:"config"`
		}

		var backup BackupData
		if err := json.NewDecoder(r.Body).Decode(&backup); err != nil {
			nethttp.Error(w, err.Error(), nethttp.StatusBadRequest)
			return
		}

		studentProfilesMutex.Lock()
		configMutex.Lock()
		studentProfiles = backup.Profiles
		globalConfig = backup.Config
		saveStudentProfiles()
		saveConfig()
		configMutex.Unlock()
		studentProfilesMutex.Unlock()

		AddLog("system", "Backup konfigurasi berhasil diimpor! Total profil: %d", len(backup.Profiles))
		w.WriteHeader(nethttp.StatusOK)
		w.Write([]byte("success"))
	})))

	// API: Event Source (SSE) Log Stream
	nethttp.Handle("/api/logs/stream", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		flusher, ok := w.(nethttp.Flusher)
		if !ok {
			nethttp.Error(w, "SSE Streaming tidak didukung!", nethttp.StatusInternalServerError)
			return
		}

		// Stream past history logs instantly
		logMutex.Lock()
		for _, msg := range logCache {
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
		}
		logMutex.Unlock()
		flusher.Flush()

		// Register new active SSE listener
		ch := make(chan LogMessage, 50)
		listenerMu.Lock()
		listeners[ch] = true
		listenerMu.Unlock()

		defer func() {
			listenerMu.Lock()
			delete(listeners, ch)
			listenerMu.Unlock()
			close(ch)
		}()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case msg := <-ch:
				data, _ := json.Marshal(msg)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
			case <-ticker.C:
				// Keep connection alive
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})))

	// Serve Frontend Static files via embed
	nethttp.Handle("/static/", basicAuthMiddleware(nethttp.FileServer(nethttp.FS(staticFS))))

	// Serve Frontend Root
	nethttp.Handle("/", basicAuthMiddleware(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.URL.Path != "/" {
			nethttp.NotFound(w, r)
			return
		}
		indexHTML, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			nethttp.Error(w, "Static asset not found", nethttp.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})))

	port := os.Getenv("PORT")
	if port == "" {
		port = "7860" // default port
	}

	AddLog("system", "Memulai web dashboard di http://localhost:%s...", port)
	err := nethttp.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatalf("❌ Web Server gagal dijalankan: %v", err)
	}
}
