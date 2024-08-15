package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	baseURL          = "https://www.pornpics.com"
	popularAPI       = "/popular/"
	searchAPI        = "/search/srch.php"
	query            = "tessa taylor"
	imageDir         = "dataset"
	limitPerPage     = 5
	maxConcurrentReq = 10
	offsetFile       = "offset.txt"
)

type ImageInfo struct {
	GalleryURL string `json:"g_url"`
	Desc       string `json:"desc"`
}

func main() {
	fmt.Println("Starting PornPics Dataset Generator...")
	offsetFileLoc := offsetFile
	if query != "" {
		offsetFileLoc = strings.Replace(offsetFileLoc, ".txt", "-"+query+".txt", 1)
	}
	os.MkdirAll(imageDir, 0755)

	offset, err := loadOffset(offsetFileLoc)
	if err != nil {
		fmt.Println("Error loading offset:", err)
		offset = 1
	}

	for {
		fmt.Printf("Fetching images from offset %d...\n", offset)
		imgPath := popularAPI
		if query != "" {
			imgPath = searchAPI
		}

		imageInfos, err := fetchImages(imgPath, query, limitPerPage, offset)
		if err != nil {
			fmt.Println("Error fetching popular images:", err)
			time.Sleep(5 * time.Second) // Wait and retry on error
			continue
		}

		fmt.Printf("Fetched %d images. Processing galleries...\n", len(imageInfos))

		var wg sync.WaitGroup
		sem := make(chan struct{}, maxConcurrentReq)
		for _, info := range imageInfos {
			wg.Add(1)
			sem <- struct{}{}
			go func(info ImageInfo) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := processGallery(info); err != nil {
					fmt.Println("Error processing gallery:", err)
				}
			}(info)
		}
		wg.Wait()

		offset += limitPerPage

		if err := saveOffset(offset, offsetFileLoc); err != nil {
			fmt.Println("Error saving offset:", err)
		}

		fmt.Printf("Offset updated to %d. Continuing...\n", offset)

		if len(imageInfos) < limitPerPage {
			fmt.Println("Reached the end of available images. Stopping.")
			break
		}
	}

	fmt.Println("Finished generating dataset.")
}

func fetchImages(imgPath, query string, limit, offset int) ([]ImageInfo, error) {
	url := fmt.Sprintf("%s%s?q=%s&lang=en&limit=%d&offset=%d", baseURL, imgPath, url.QueryEscape(query), limit, offset)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var imageInfos []ImageInfo
	if err := json.NewDecoder(resp.Body).Decode(&imageInfos); err != nil {
		return nil, err
	}
	return imageInfos, nil
}

func processGallery(info ImageInfo) error {
	resp, err := http.Get(info.GalleryURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return err
	}

	categories := extractCategories(doc)
	tags := extractTags(doc)
	models := extractModels(doc)
	channels := extractChannels(doc)

	if len(channels) < 2 {
		fmt.Println("No channels found for gallery:", info.GalleryURL)
		return nil
	}

	categoryDir := path.Join(imageDir, channels[1])
	os.MkdirAll(categoryDir, 0755)

	doc.Find("#tiles .thumbwook img").Each(func(i int, img *goquery.Selection) {
		imageURL, _ := img.Attr("data-src")
		imageDesc, _ := img.Attr("alt")

		if err := downloadImage(imageURL, categoryDir, imageDesc, categories, tags, models, channels); err != nil {
			fmt.Println("Error downloading image:", err)
		}
	})

	return nil
}

func extractCategories(doc *goquery.Document) []string {
	var categories []string
	doc.Find("#content > div.gallery-info.to-gall-info > div.tags:nth-child(3) > div > a > span").Each(func(i int, s *goquery.Selection) {
		categories = append(categories, s.Text())
	})
	return categories
}

func extractTags(doc *goquery.Document) []string {
	var tags []string
	doc.Find("a[href^=\"/tags\"] > span").Each(func(i int, s *goquery.Selection) {
		tags = append(tags, s.Text())
	})
	return tags
}

func extractModels(doc *goquery.Document) []string {
	var models []string
	doc.Find("a[href^=\"/pornstars\"] > span").Each(func(i int, s *goquery.Selection) {
		models = append(models, s.Text())
	})
	return models
}

func extractChannels(doc *goquery.Document) []string {
	var channels []string
	doc.Find("a[href^=\"/channels\"]").Each(func(i int, s *goquery.Selection) {
		channels = append(channels, s.Text())
	})
	return channels
}

func downloadImage(url, dir, desc string, categories, tags []string, models []string, channels []string) error {
	hd_url := strings.Replace(url, "cdni.pornpics.com/460", "cdni.pornpics.com/1280", 1)
	resp, err := http.Get(hd_url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	filename := path.Join(dir, path.Base(hd_url))

	if _, err := os.Stat(filename); err == nil {
		return nil // Skip downloading if it exists
	}

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return err
	}

	prompt := fmt.Sprintf("%s, Categories: %s, Tags: %s, Models: %s, Channels: %s", desc, strings.Join(categories, ", "), strings.Join(tags, ", "), strings.Join(models, ", "), strings.Join(channels[1:], ", "))
	err = os.WriteFile(filename+".txt", []byte(prompt), 0644)
	if err != nil {
		return err
	}

	return nil
}

func loadOffset(offsetFileLoc string) (int, error) {
	file, err := os.Open(offsetFileLoc)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 1, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		var offset int
		if _, err := fmt.Sscan(scanner.Text(), &offset); err != nil {
			return 1, err
		}
		return offset, nil
	}

	return 1, nil
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
