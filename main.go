package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	baseURL   = "https://www.pornpics.com"
	searchAPI = "/search/srch.php"
	popularAPI = "/tags/"
)

// Config holds all runtime parameters — no more recompiling for a query change.
type Config struct {
	Query          string
	ImageDir       string
	LimitPerPage   int
	MaxConcurrent  int
	OffsetFile     string
	RetryAttempts  int
	RequestDelay   time.Duration
	RequestTimeout time.Duration
}

type ImageInfo struct {
	GalleryURL string `json:"g_url"`
	Desc       string `json:"desc"`
}

// Stats tracks runtime metrics for visibility into what's actually happening.
type Stats struct {
	Downloaded  atomic.Int64
	Skipped     atomic.Int64
	Failed      atomic.Int64
	Galleries   atomic.Int64
}

func main() {
	cfg := parseFlags()

	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Printf("Starting scraper | query=%q dir=%s concurrency=%d", cfg.Query, cfg.ImageDir, cfg.MaxConcurrent)

	if err := os.MkdirAll(cfg.ImageDir, 0755); err != nil {
		log.Fatalf("Cannot create image dir: %v", err)
	}

	offsetFileLoc := resolveOffsetFile(cfg)

	offset, err := loadOffset(offsetFileLoc)
	if err != nil {
		log.Printf("No offset file found, starting from 0")
		offset = 0
	}

	// Shared HTTP client with timeout — not the default client.
	client := &http.Client{
		Timeout: cfg.RequestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        cfg.MaxConcurrent * 2,
			MaxIdleConnsPerHost: cfg.MaxConcurrent,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Failed gallery log — audit trail so you know what to retry.
	failLog, err := os.OpenFile("failed_galleries.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Cannot open fail log: %v", err)
	}
	defer failLog.Close()
	failMu := &sync.Mutex{}

	stats := &Stats{}

	// Rate limiter: a ticker that gates requests to avoid IP bans.
	rateTicker := time.NewTicker(cfg.RequestDelay)
	defer rateTicker.Stop()

	for {
		<-rateTicker.C

		log.Printf("Fetching offset=%d", offset)
		apiPath := popularAPI
		if cfg.Query != "" {
			apiPath = searchAPI
		}

		imageInfos, err := fetchImagesWithRetry(client, apiPath, cfg.Query, cfg.LimitPerPage, offset, cfg.RetryAttempts)
		if err != nil {
			log.Printf("ERROR fetching images at offset %d: %v — retrying in 10s", offset, err)
			time.Sleep(10 * time.Second)
			continue
		}

		if len(imageInfos) == 0 {
			log.Println("No images returned. End of results.")
			break
		}

		log.Printf("Got %d galleries to process", len(imageInfos))

		var wg sync.WaitGroup
		sem := make(chan struct{}, cfg.MaxConcurrent)

		for _, info := range imageInfos {
			wg.Add(1)
			sem <- struct{}{}

			go func(info ImageInfo) {
				defer wg.Done()
				defer func() { <-sem }()

				stats.Galleries.Add(1)

				if err := processGallery(client, info, &cfg, stats); err != nil {
					log.Printf("GALLERY FAIL %s: %v", info.GalleryURL, err)
					failMu.Lock()
					fmt.Fprintf(failLog, "%s\t%v\n", info.GalleryURL, err)
					failMu.Unlock()
					stats.Failed.Add(1)
				}
			}(info)
		}

		wg.Wait()

		offset += cfg.LimitPerPage
		if err := saveOffset(offset, offsetFileLoc); err != nil {
			log.Printf("ERROR saving offset: %v", err)
		}

		log.Printf("Progress — offset=%d downloaded=%d skipped=%d failed=%d",
			offset,
			stats.Downloaded.Load(),
			stats.Skipped.Load(),
			stats.Failed.Load(),
		)

		if len(imageInfos) < cfg.LimitPerPage {
			log.Println("Last page reached. Done.")
			break
		}
	}

	log.Printf("Finished. Total galleries=%d downloaded=%d skipped=%d failed=%d",
		stats.Galleries.Load(),
		stats.Downloaded.Load(),
		stats.Skipped.Load(),
		stats.Failed.Load(),
	)
}

func parseFlags() Config {
	cfg := Config{}
	flag.StringVar(&cfg.Query, "query", "", "Search query (empty = popular)")
	flag.StringVar(&cfg.ImageDir, "dir", "dataset/", "Output directory for images")
	flag.IntVar(&cfg.LimitPerPage, "limit", 20, "Results per page")
	flag.IntVar(&cfg.MaxConcurrent, "concurrency", 5, "Max concurrent gallery downloads")
	flag.StringVar(&cfg.OffsetFile, "offset-file", "", "Override offset file path")
	flag.IntVar(&cfg.RetryAttempts, "retries", 3, "HTTP retry attempts per request")
	delayMs := flag.Int("delay-ms", 500, "Delay between page fetches in milliseconds")
	timeoutSec := flag.Int("timeout", 30, "HTTP request timeout in seconds")
	flag.Parse()

	cfg.RequestDelay = time.Duration(*delayMs) * time.Millisecond
	cfg.RequestTimeout = time.Duration(*timeoutSec) * time.Second
	return cfg
}

func resolveOffsetFile(cfg Config) string {
	if cfg.OffsetFile != "" {
		return cfg.OffsetFile
	}
	if cfg.Query != "" {
		safe := strings.NewReplacer(" ", "_", "/", "_").Replace(cfg.Query)
		return fmt.Sprintf("offset-%s.txt", safe)
	}
	return "offset.txt"
}

// fetchImagesWithRetry wraps fetchImages with exponential backoff.
func fetchImagesWithRetry(client *http.Client, apiPath, query string, limit, offset, retries int) ([]ImageInfo, error) {
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			log.Printf("Retry %d/%d after %s", attempt, retries, backoff)
			time.Sleep(backoff)
		}
		results, err := fetchImages(client, apiPath, query, limit, offset)
		if err == nil {
			return results, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all %d attempts failed: %w", retries, lastErr)
}

func fetchImages(client *http.Client, apiPath, query string, limit, offset int) ([]ImageInfo, error) {
	u := fmt.Sprintf("%s%s?q=%s&lang=en&limit=%d&offset=%d",
		baseURL, apiPath, url.QueryEscape(query), limit, offset)

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	// Minimal browser-like headers to avoid trivial bot detection.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, u)
	}

	var imageInfos []ImageInfo
	if err := json.NewDecoder(resp.Body).Decode(&imageInfos); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}
	return imageInfos, nil
}

func processGallery(client *http.Client, info ImageInfo, cfg *Config, stats *Stats) error {
	resp, err := client.Get(info.GalleryURL)
	if err != nil {
		return fmt.Errorf("GET gallery: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gallery returned %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return fmt.Errorf("parse HTML: %w", err)
	}

	categories := extractCategories(doc)
	tags := extractTags(doc)
	models := extractModels(doc)
	channels := extractChannels(doc)

	// Use gallery URL slug as fallback directory if no channel found.
	dirName := "unknown"
	if len(channels) >= 2 {
		dirName = sanitizeDirName(channels[1])
	} else {
		slug := path.Base(strings.TrimRight(info.GalleryURL, "/"))
		if slug != "" && slug != "." {
			dirName = sanitizeDirName(slug)
		}
		log.Printf("No channel for %s — using dir %q", info.GalleryURL, dirName)
	}

	categoryDir := path.Join(cfg.ImageDir, dirName)
	if err := os.MkdirAll(categoryDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", categoryDir, err)
	}

	doc.Find("#tiles .thumbwook img").Each(func(i int, img *goquery.Selection) {
		imageURL, exists := img.Attr("data-src")
		if !exists || imageURL == "" {
			return
		}
		imageDesc, _ := img.Attr("alt")

		err := downloadImageWithRetry(client, imageURL, categoryDir, imageDesc,
			categories, tags, models, channels, cfg.RetryAttempts, stats)
		if err != nil {
			log.Printf("DOWNLOAD FAIL %s: %v", imageURL, err)
			stats.Failed.Add(1)
		}
	})

	return nil
}

func downloadImageWithRetry(client *http.Client, rawURL, dir, desc string,
	categories, tags, models, channels []string, retries int, stats *Stats) error {

	hdURL := toHDURL(rawURL)
	filename := path.Join(dir, path.Base(hdURL))

	// Check existence before any network call.
	if _, err := os.Stat(filename); err == nil {
		stats.Skipped.Add(1)
		return nil
	}

	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		err := downloadImage(client, hdURL, filename, desc, categories, tags, models, channels)
		if err == nil {
			stats.Downloaded.Add(1)
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("download failed after %d attempts: %w", retries, lastErr)
}

func downloadImage(client *http.Client, hdURL, filename, desc string,
	categories, tags, models, channels []string) error {

	resp, err := client.Get(hdURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("image returned %d", resp.StatusCode)
	}

	// Write to a temp file first — avoids partial writes on crash.
	tmpFile := filename + ".tmp"
	file, err := os.Create(tmpFile)
	if err != nil {
		return err
	}

	if _, err = io.Copy(file, resp.Body); err != nil {
		file.Close()
		os.Remove(tmpFile)
		return err
	}
	file.Close()

	if err := os.Rename(tmpFile, filename); err != nil {
		os.Remove(tmpFile)
		return err
	}

	// Build prompt string for OneTrainer.
	var parts []string
	if desc != "" {
		parts = append(parts, desc)
	}
	if len(categories) > 0 {
		parts = append(parts, "Categories: "+strings.Join(categories, ", "))
	}
	if len(tags) > 0 {
		parts = append(parts, "Tags: "+strings.Join(tags, ", "))
	}
	if len(models) > 0 {
		parts = append(parts, "Models: "+strings.Join(models, ", "))
	}
	if len(channels) > 1 {
		parts = append(parts, "Channels: "+strings.Join(channels[1:], ", "))
	}
	prompt := strings.Join(parts, ", ")

	return os.WriteFile(filename+".txt", []byte(prompt), 0644)
}

// toHDURL swaps the CDN resolution path from 460 to 1280.
func toHDURL(rawURL string) string {
	return strings.Replace(rawURL, "cdni.pornpics.com/460", "cdni.pornpics.com/1280", 1)
}

// sanitizeDirName strips characters that break filesystem paths.
func sanitizeDirName(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return strings.TrimSpace(r.Replace(name))
}

func extractCategories(doc *goquery.Document) []string {
	var out []string
	doc.Find("#content > div.gallery-info.to-gall-info > div.tags:nth-child(3) > div > a > span").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			out = append(out, t)
		}
	})
	return out
}

func extractTags(doc *goquery.Document) []string {
	var out []string
	doc.Find("a[href^=\"/tags\"] > span").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			out = append(out, t)
		}
	})
	return out
}

func extractModels(doc *goquery.Document) []string {
	var out []string
	doc.Find("a[href^=\"/pornstars\"] > span").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			out = append(out, t)
		}
	})
	return out
}

func extractChannels(doc *goquery.Document) []string {
	var out []string
	doc.Find("a[href^=\"/channels\"]").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			out = append(out, t)
		}
	})
	return out
}

func loadOffset(offsetFileLoc string) (int, error) {
	file, err := os.Open(offsetFileLoc)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		var offset int
		if _, err := fmt.Sscan(scanner.Text(), &offset); err != nil {
			return 0, err
		}
		return offset, nil
	}
	return 0, nil
}

func saveOffset(offset int, offsetFileLoc string) error {
	file, err := os.Create(offsetFileLoc)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%d\n", offset)
	return err
}