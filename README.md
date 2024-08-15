# PornPics Dataset Generator

This Go application scrapes images and their associated metadata from PornPics to create a labeled dataset suitable for fine-tuning Stable Diffusion models.

## Features

- Fetches popular images from PornPics using the provided API.
- Downloads images from each gallery and saves them in a structured directory.
- Extracts categories, tags, models & channels from the gallery page and includes them in the dataset.
- Creates a text file for each image containing the prompt (alt text, categories, and tags) for [OneTrainer](https://github.com/Nerogar/OneTrainer).
- Persists the current offset to allow resuming the scraping process on subsequent runs.
- Utilizes concurrency to improve performance.

## Requirements

- Go (version 1.16 or higher)
- `goquery` library: `go get github.com/PuerkitoBio/goquery`

## Usage

1. **Clone the repository:**

   ```bash
   git clone https://github.com/CrudeCreations/pornpics-dataset-gen
   cd pornpics-dataset-gen
   ```

2. **Install dependencies**

    ```bash
    go get github.com/PuerkitoBio/goquery
    ```

3. **Run the application**

    ```bash
    go run main.go
    ```
- The application will start fetching and processing images.
- Images will be saved in the dataset directory, organized into subdirectories based on categories.
- The current offset will be saved to offset.txt after each page.
- You can stop the application at any time (e.g., with Ctrl+C), and it will resume from the last saved offset - when you run it again.

## Disclaimer
`This application is provided for educational and research purposes only. The author is not responsible for any misuse or consequences arising from its use.`