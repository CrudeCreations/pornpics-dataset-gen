package main

import (
	"encoding/json"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	imageDir       = "../dataset" // Directory containing images and labels
	refinedDir     = "../refined" // Directory for refined images and labels
	labelExtension = ".txt"       // Extension for label files
	processedFile  = "processed.json"
)

type ImageData struct {
	Filename string
	Label    string
}

type ProcessedImageData struct {
	Confirmed bool `json:"confirmed"`
	Skipped   bool `json:"skipped"`
	ID        int  `json:"id"`
}

func main() {
	// Load processed images from file
	processedImages := loadProcessedImages()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Get current image index or default to 0
		indexStr := r.URL.Query().Get("index")
		update := r.URL.Query().Get("update")
		index, err := strconv.Atoi(indexStr)
		if err != nil || index < 0 {
			index = 0
		}

		// Get image filenames
		imageFiles, err := getImageFiles()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if r.URL.Query().Get("random") == "true" {
			index = getRandomUnprocessedImageIndex(imageFiles, processedImages)
		} else {
			// Get current image index or default to 0
			indexStr := r.URL.Query().Get("index")
			var err error
			index, err = strconv.Atoi(indexStr)
			if err != nil || index < 0 {
				// If index is invalid or not provided, find the first unprocessed image
				index = getNextUnprocessedImageIndex(imageFiles, processedImages, 0)
			} else if update == "" {
				// If index is provided, find the next unprocessed image from that index
				index = getNextUnprocessedImageIndex(imageFiles, processedImages, index)
			}

			// Check if there are any unprocessed images left
			if index == -1 {
				http.Error(w, "No more unprocessed images", http.StatusNotFound)
				return
			}
		}

		// Check if index is within bounds
		if index >= len(imageFiles) {
			http.Error(w, "Image not found", http.StatusNotFound)
			return
		}

		// Get image data
		imageData, err := loadImageData(imageFiles[index])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		processedImageData, ok := processedImages[imageData.Filename]
		if !ok {
			processedImageData = ProcessedImageData{}
		}

		funcMap := template.FuncMap{
			"add": func(a, b int) int {
				return a + b
			},
			"sub": func(a, b int) int {
				return a - b
			},
		}

		// Render the template
		tmpl, err := template.New("template.html").Funcs(funcMap).ParseFiles("template.html") // Assuming you have a template.html file
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		err = tmpl.Execute(w, struct {
			ImageData       ImageData
			Index           int
			TotalImages     int
			ProcessedImages map[string]ProcessedImageData
			Confirmed       bool
			Skipped         bool
		}{
			imageData,
			index,
			len(imageFiles),
			processedImages,
			processedImageData.Confirmed,
			processedImageData.Skipped,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})

	http.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		filename := r.FormValue("filename")
		label := r.FormValue("label")
		confirmed := r.FormValue("confirmed") == "on"
		skipped := r.FormValue("skip") == "on"
		index, err := strconv.Atoi(r.FormValue("index"))
		if err != nil {
			http.Error(w, "ID is invalid", http.StatusBadRequest)
			return
		}
		if confirmed {
			err := saveToRefinedDataset(filename, label)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		processedImages[filename] = ProcessedImageData{
			Confirmed: confirmed,
			Skipped:   skipped,
			ID:        index,
		}
		saveProcessedImages(processedImages)

		imageFiles, err := getImageFiles()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Redirect to the next unprocessed image
		newIndex := getNextUnprocessedImageIndex(imageFiles, processedImages, 0)
		if newIndex == -1 {
			http.Error(w, "No more unprocessed images", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/?index="+strconv.Itoa(newIndex), http.StatusSeeOther)
	})

	http.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		// Extract the image filename from the request path
		file := filepath.ToSlash(strings.Replace(r.URL.Path, "/images", "", 1))
		// Construct the full image path
		imagePath := filepath.Join(imageDir, file)

		// Serve the image file
		http.ServeFile(w, r, imagePath)
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}

func getImageFiles() ([]string, error) {
	var imageFiles []string

	err := filepath.Walk(imageDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !strings.HasSuffix(info.Name(), labelExtension) {
			// Store relative path from imageDir
			relPath, err := filepath.Rel(imageDir, path)
			if err != nil {
				return err
			}
			imageFiles = append(imageFiles, relPath)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return imageFiles, nil
}

func loadImageData(filename string) (ImageData, error) {
	labelFile := filepath.Join(imageDir, filename+labelExtension)
	labelBytes, err := os.ReadFile(labelFile)
	if err != nil {
		// If label file doesn't exist, assume empty label
		if os.IsNotExist(err) {
			return ImageData{Filename: filename, Label: ""}, nil
		}
		return ImageData{}, err
	}
	return ImageData{Filename: filename, Label: string(labelBytes)}, nil
}

func saveToRefinedDataset(filename, label string) error {
	// Get the relative directory path from imageDir
	relDir := filepath.Dir(filename)

	// Create the corresponding subdirectory in refinedDir
	dstDirPath := filepath.Join(refinedDir, relDir)
	if err := os.MkdirAll(dstDirPath, 0755); err != nil {
		return err
	}

	// Copy image file
	srcImagePath := filepath.Join(imageDir, filename)
	dstImagePath := filepath.Join(refinedDir, filename)
	if err := copyFile(srcImagePath, dstImagePath); err != nil {
		return err
	}

	// Save label file
	labelFile := filepath.Join(refinedDir, filename+labelExtension)
	if err := os.WriteFile(labelFile, []byte(label), 0644); err != nil {
		return err
	}

	return nil
}

func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	err = os.WriteFile(dst, input, 0644)
	if err != nil {
		return err
	}

	return nil
}

func loadProcessedImages() map[string]ProcessedImageData {
	processed := make(map[string]ProcessedImageData)
	data, err := os.ReadFile(processedFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error loading processed images: %v", err)
		}
		return processed
	}
	if err := json.Unmarshal(data, &processed); err != nil {
		log.Printf("Error unmarshaling processed images: %v", err)
	}
	return processed
}

func saveProcessedImages(processed map[string]ProcessedImageData) {
	data, err := json.Marshal(processed)
	if err != nil {
		log.Printf("Error marshaling processed images: %v", err)
		return
	}
	if err := os.WriteFile(processedFile, data, 0644); err != nil {
		log.Printf("Error saving processed images: %v", err)
	}
}

func getNextUnprocessedImageIndex(imageFiles []string, processedImages map[string]ProcessedImageData, startIndex int) int {
	for i := startIndex; i < len(imageFiles); i++ {
		if !processedImages[imageFiles[i]].Confirmed && !processedImages[imageFiles[i]].Skipped {
			return i
		}
	}
	return -1 // No more unprocessed images
}

func getRandomUnprocessedImageIndex(imageFiles []string, processedImages map[string]ProcessedImageData) int {
	unprocessedFiles := []string{}
	for _, file := range imageFiles {
		if !processedImages[file].Confirmed && !processedImages[file].Skipped {
			unprocessedFiles = append(unprocessedFiles, file)
		}
	}
	if len(unprocessedFiles) == 0 {
		return -1 // No unprocessed images
	}
	return rand.Intn(len(unprocessedFiles))
}
