package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	nethttp "net/http"
	"net/url"
	"os"
	"path/filepath"
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
	USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// Data Models
type Profile struct {
	NIM           string    `json:"nim"`
	Name          string    `json:"name"`
	CookiesJSON   string    `json:"cookies_json"`
	WAToken       string    `json:"wa_token"`
	WAGroup       string    `json:"wa_group"`
	IsActive      bool      `json:"is_active"`
	LastRunStatus string    `json:"last_run_status"` // "success", "failed", "no_link", "expired", or ""
	LastRunTime   time.Time `json:"last_run_time"`
	LastRunMsg    string    `json:"last_run_msg"`
	FailedCount   int       `json:"failed_count"`
}

type GlobalConfig struct {
	WAToken       string `json:"wa_token"`
	WAGroup       string `json:"wa_group"`
	CheckInterval int    `json:"check_interval"` // in minutes
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

// Database Operations
func loadData() {
	// Try loading config
	cfgData, err := os.ReadFile("config.json")
	if err == nil {
		json.Unmarshal(cfgData, &globalConfig)
	} else {
		// Migrate legacy env values
		globalConfig.WAToken = os.Getenv("WA_TOKEN")
		globalConfig.WAGroup = os.Getenv("WA_GROUP")
		saveConfig()
	}

	// Try loading profiles
	profData, err := os.ReadFile("profiles.json")
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
	os.WriteFile("profiles.json", data, 0644)
}

func saveConfig() {
	data, _ := json.MarshalIndent(globalConfig, "", "  ")
	os.WriteFile("config.json", data, 0644)
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
		req.Header.Set("sec-ch-ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)
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
		clientProfile = profiles.Chrome_120
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
		} else {
			p.LastRunMsg = fmt.Sprintf("Cloudflare memblokir request (HTTP 403) (Percobaan %d/3)", p.FailedCount)
		}
		AddLog("error", "NIM %s: Cloudflare memblokir akses (HTTP 403) (Percobaan %d/3). Ganti Cookies Anda.", p.NIM, p.FailedCount)
		return nil
	}

	if resp.StatusCode == 301 || resp.StatusCode == 302 {
		p.FailedCount++
		p.LastRunTime = time.Now()
		if p.FailedCount >= 3 {
			p.LastRunStatus = "expired"
			p.LastRunMsg = "Sesi kedaluwarsa (Redirect ke Login) (Gagal 3 kali)"
		} else {
			p.LastRunMsg = fmt.Sprintf("Sesi kedaluwarsa (Redirect ke Login) (Percobaan %d/3)", p.FailedCount)
		}
		AddLog("warning", "NIM %s: Sesi kedaluwarsa (Percobaan %d/3), silakan update Cookies.", p.NIM, p.FailedCount)
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
	for _, p := range studentProfiles {
		if p.IsActive {
			activeProfiles = append(activeProfiles, p)
		}
	}
	studentProfilesMutex.RUnlock()

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
			for j, dbP := range studentProfiles {
				if dbP.NIM == p.NIM {
					studentProfiles[j] = *p
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
				AddLog("system", "Pukul %s WIB: Di luar jam operasional (%02d:00 - %02d:00). Scheduler tidur 10 menit...",
					now.Format("15:04:05"), startHour, endHour)
				time.Sleep(10 * time.Minute)
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
			if newP.NIM == "" || newP.Name == "" || newP.CookiesJSON == "" {
				nethttp.Error(w, "NIM, Nama, dan Cookies wajib diisi!", nethttp.StatusBadRequest)
				return
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
					studentProfiles[i] = newP
					found = true
					break
				}
			}
			if !found {
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

					AddLog("system", "Menjalankan paksa bot presensi NIM %s...", pCopy.NIM)
					successes := runBotForProfile(&pCopy)

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
					studentProfiles[targetIdx] = pCopy
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
