package main

import (
	"encoding/json"
	"fmt"
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

var (
	NIM        string
	WA_TOKEN   string
	WA_GROUP   string
	URL_JADWAL = "https://infokhs.umm.ac.id/jadwal-kuliah"
	USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// Struktur Cookie untuk parsing JSON
type CookieItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type CookieJSON struct {
	UserAgent string       `json:"user_agent"`
	Cookies   []CookieItem `json:"cookies"`
}

// Fungsi kirim WA menggunakan API Fonnte
func sendWhatsApp(message string) {
	if WA_TOKEN == "" || WA_GROUP == "" {
		fmt.Println("WA_TOKEN atau WA_GROUP belum diatur. Melewati pengiriman WhatsApp.")
		return
	}

	client := &http.Client{}
	data := url.Values{}
	data.Set("target", WA_GROUP)
	data.Set("message", message)
	data.Set("delay", "1")

	req, err := http.NewRequest("POST", "https://api.fonnte.com/send", strings.NewReader(data.Encode()))
	if err != nil {
		fmt.Printf("Error membuat request WA: %v\n", err)
		return
	}
	req.Header.Set("Authorization", WA_TOKEN)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error mengirim WhatsApp: %v\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println("Pesan WhatsApp berhasil dikirim.")
}

// Fungsi muat Cookie
func loadCookies() string {
	envCookies := os.Getenv("COOKIES_JSON")
	var cookieString string

	parseCookies := func(data []byte) string {
		var cj CookieJSON
		if err := json.Unmarshal(data, &cj); err == nil && len(cj.Cookies) > 0 {
			if cj.UserAgent != "" {
				USER_AGENT = cj.UserAgent
			}
			var pairs []string
			for _, c := range cj.Cookies {
				pairs = append(pairs, c.Name+"="+c.Value)
			}
			return strings.Join(pairs, "; ")
		}

		// Fallback jika format array langsung
		var cl []CookieItem
		if err := json.Unmarshal(data, &cl); err == nil {
			var pairs []string
			for _, c := range cl {
				pairs = append(pairs, c.Name+"="+c.Value)
			}
			return strings.Join(pairs, "; ")
		}
		return ""
	}

	if envCookies != "" {
		cookieString = parseCookies([]byte(envCookies))
		if cookieString != "" {
			return cookieString
		}
		fmt.Println("Gagal memuat cookies dari env COOKIES_JSON, mencoba dari file...")
	}

	// Coba cari cookies_umm.json di current folder, kalau gak ada cari di parent folder
	cookieFile := filepath.Join(".", "cookies_umm.json")
	if _, err := os.Stat(cookieFile); os.IsNotExist(err) {
		cookieFile = filepath.Join("..", "cookies_umm.json")
	}

	data, err := os.ReadFile(cookieFile)
	if err != nil {
		fmt.Printf("❌ Gagal memuat cookies dari file: %v\n", err)
		return ""
	}

	return parseCookies(data)
}

// Fungsi helper request dengan TLS Client (Bypass CF)
func makeRequest(client tls_client.HttpClient, targetUrl string, cookieHeader string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, targetUrl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", USER_AGENT)
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
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

func runBot() {
	fmt.Println("============================================================")
	fmt.Println("🚀 UMM INFOKHS API BOT - GOLANG EDITION (ULTRA FAST)")
	fmt.Println("============================================================")

	// Load dari file env (folder parent atau saat ini)
	godotenv.Load("../.env")
	godotenv.Load(".env")

	NIM = os.Getenv("NIM")
	WA_TOKEN = os.Getenv("WA_TOKEN")
	WA_GROUP = os.Getenv("WA_GROUP")

	if NIM == "" {
		fmt.Println("❌ NIM belum diatur di file .env!")
		return
	}

	cookieHeader := loadCookies()
	if cookieHeader == "" {
		fmt.Println("❌ Cookie gagal dimuat. Pastikan cookies_umm.json ada atau COOKIES_JSON terisi.")
		return
	}

	fmt.Printf("🆔 NIM     : %s\n", NIM)
	fmt.Println("🔑 Cookies : Dimuat dengan sukses.")
	fmt.Printf("🕒 Mulai   : %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	// Setup Engine Impersonate TLS untuk membodohi Cloudflare
	// Karena cookie Anda berasal dari Firefox, kita harus menyamar menjadi Firefox!
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Firefox_132),
		tls_client.WithNotFollowRedirects(),
	}

	client, err := tls_client.NewHttpClient(tls_client.NewLogger(), options...)
	if err != nil {
		log.Fatalf("Gagal inisialisasi TLS Client: %v", err)
	}

	fmt.Println("🌐 Mengakses halaman jadwal kuliah...")
	resp, err := makeRequest(client, URL_JADWAL, cookieHeader)
	if err != nil {
		fmt.Printf("❌ Gagal request jadwal: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 {
		fmt.Println("❌ HTTP Error 403: Forbidden")
		fmt.Println("⚠️  Terdeteksi blokir Cloudflare. Cookies mungkin salah/expired.")
		return
	}

	if resp.StatusCode == 301 || resp.StatusCode == 302 {
		fmt.Println("⚠️  Sesi Anda telah kedaluwarsa (dialihkan ke halaman login)!")
		return
	}

	// Parsing HTML Jadwal
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		fmt.Printf("❌ Gagal parse jadwal HTML: %v\n", err)
		return
	}

	var presenceLinks []string
	doc.Find("a.btn-success, a.btn-xs").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if exists && (strings.Contains(href, "presensi") || strings.Contains(href, "jadwal-kuliah/presensi")) {
			if strings.HasPrefix(href, "/") {
				href = "https://infokhs.umm.ac.id" + href
			} else if !strings.HasPrefix(href, "http") {
				href = "https://infokhs.umm.ac.id/" + href
			}

			// Cek duplikasi array
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

	fmt.Printf("📋 Ditemukan %d tautan presensi mata kuliah.\n", len(presenceLinks))

	if len(presenceLinks) == 0 {
		fmt.Println("⚠️ Tidak ada tautan presensi matkul aktif di jadwal.")
		return
	}

	totalSuccess := 0
	var matkulBerhasil []string
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, link := range presenceLinks {
		wg.Add(1)
		go func(idx int, targetLink string) {
			defer wg.Done()

			fmt.Printf("\n🔄 [%d/%d] Membuka halaman presensi...\n", idx+1, len(presenceLinks))

			pResp, pErr := makeRequest(client, targetLink, cookieHeader)
			if pErr != nil || pResp.StatusCode != 200 {
				fmt.Printf("   ❌ [%d] Gagal memuat detail presensi.\n", idx+1)
				if pResp != nil {
					pResp.Body.Close()
				}
				return
			}

			pDoc, err := goquery.NewDocumentFromReader(pResp.Body)
			pResp.Body.Close()
			if err != nil {
				fmt.Printf("   ❌ [%d] Gagal parse HTML presensi.\n", idx+1)
				return
			}

			// Ekstrak Nama Mata Kuliah
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

			fmt.Printf("   📚 [%d] Mata Kuliah: %s\n", idx+1, courseName)

			// Parse Table Headers Dinamis
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

			// Jika tidak ketemu, pakai default index
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

			// Cari Tombol Presensi
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

			if alreadyVCount > 0 {
				fmt.Printf("   ℹ️  [%d] %d baris sudah terisi 'v' (Hadir) → SKIP\n", idx+1, alreadyVCount)
			}

			if len(buttonsToClick) == 0 {
				fmt.Printf("   ✅ [%d] Tidak ada tombol presensi baru yang perlu diklik.\n", idx+1)
				return
			}

			fmt.Printf("   🔥 [%d] Ditemukan %d tombol presensi AKTIF!\n", idx+1, len(buttonsToClick))

			// Eksekusi Klik Hadir!
			clickCount := 0
			var successMsg []string

			for _, btn := range buttonsToClick {
				fmt.Printf("   ⚡ [%d] Mengirim request presensi untuk baris %s...\n", idx+1, btn["rIdx"])

				aResp, aErr := makeRequest(client, btn["link"], cookieHeader)
				if aErr != nil {
					fmt.Printf("   ❌ [%d] Error saat klik presensi.\n", idx+1)
					continue
				}
				if aResp.StatusCode == 301 || aResp.StatusCode == 302 {
					fmt.Printf("   ⚠️ [%d] Session expired saat klik.\n", idx+1)
					aResp.Body.Close()
					return
				}
				aResp.Body.Close()

				fmt.Printf("   ✅ [%d] Sukses memproses presensi baris %s!\n", idx+1, btn["rIdx"])
				clickCount++

				waktuInfo := ""
				if btn["mulai"] != "" && btn["selesai"] != "" {
					waktuInfo = fmt.Sprintf(" (%s - %s)", btn["mulai"], btn["selesai"])
				}
				successMsg = append(successMsg, fmt.Sprintf("• %s%s", courseName, waktuInfo))
			}

			if clickCount > 0 {
				mu.Lock()
				totalSuccess += clickCount
				matkulBerhasil = append(matkulBerhasil, successMsg...)
				mu.Unlock()
			}
		}(i, link)
	}

	wg.Wait()

	fmt.Println("\n==================================================")
	fmt.Printf("🎉 Selesai! Berhasil melakukan %d presensi otomatis.\n", totalSuccess)
	fmt.Println("==================================================")

	if totalSuccess > 0 && len(matkulBerhasil) > 0 {
		pesan := fmt.Sprintf("Presensi\n%s\nbang", strings.Join(matkulBerhasil, "\n"))
		sendWhatsApp(pesan)
	}
}

func main() {
	// Jalankan bot pertama kali dan kemudian setiap 1 detik
	go func() {
		for {
			runBot()
			fmt.Println("\n⏳ Menunggu 10 detik sebelum pengecekan berikutnya...")
			time.Sleep(1 * time.Second)
		}
	}()

	// Setup dummy HTTP server untuk Hugging Face
	nethttp.HandleFunc("/", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.WriteHeader(nethttp.StatusOK)
		w.Write([]byte("🤖 Bot Presensi UMM is running smoothly!"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "7860" // Default port untuk Hugging Face Spaces
	}

	fmt.Printf("🌍 Memulai server web di port %s untuk Hugging Face...\n", port)
	err := nethttp.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatalf("❌ Server gagal dijalankan: %v", err)
	}
}
