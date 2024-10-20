package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/schollz/progressbar/v3"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

var (
	botToken string
	chatID   string
)

// Create a rate limiter
var limiter = rate.NewLimiter(10, 20) 

func main() {
	// Command-line flags
	urlFlag := flag.String("u", "", "Single URL to scan")
	listFlag := flag.String("l", "", "File containing list of URLs to scan")
	shellsFile := flag.String("s", "path/list_shell.txt", "File containing list of shell names")
	pathsFile := flag.String("p", "path/list_path.txt", "File containing list of URL paths")
	threads := flag.Int("threads", 150, "Number of concurrent threads") // Increased threads
	proxyFlag := flag.String("proxy", "", "HTTP proxy address (e.g. http://127.0.0.1:8080) for a single proxy")
	proxyListFlag := flag.String("proxy-list", "", "File containing list of proxies")
	flag.StringVar(&botToken, "bot-token", "", "Telegram bot token")
	flag.StringVar(&chatID, "chat-id", "", "Telegram chat ID")

	flag.Parse()

	// Load proxies if proxy list is provided
	var proxies []string
	if *proxyListFlag != "" {
		var err error
		proxies, err = loadList(*proxyListFlag)
		if err != nil {
			log.Fatalf("Failed to load proxy list: %v", err)
		}
	} else if *proxyFlag != "" {
		proxies = append(proxies, *proxyFlag)
	}

	if *urlFlag == "" && *listFlag == "" {
		log.Fatal("You must provide either a URL with -u or a list of URLs with -l")
	}

	shells, err := loadList(*shellsFile)
	if err != nil {
		log.Fatalf("Failed to load shell list: %v", err)
	}

	paths, err := loadList(*pathsFile)
	if err != nil {
		log.Fatalf("Failed to load path list: %v", err)
	}

	var urlsToScan []string
	if *urlFlag != "" {
		urlsToScan = append(urlsToScan, generateURLs(*urlFlag, shells, paths)...)
	} else if *listFlag != "" {
		urls, err := loadList(*listFlag)
		if err != nil {
			log.Fatalf("Failed to load URL list: %v", err)
		}
		for _, line := range urls {
			extractedURL := extractURLFromText(line)
			if extractedURL != "" {
				urlsToScan = append(urlsToScan, generateURLs(extractedURL, shells, paths)...)
			}
		}
	}

	bar := progressbar.Default(int64(len(urlsToScan)), "Scanning URLs")

	workerPool(urlsToScan, *threads, proxies, bar)
}

func loadList(filePath string) ([]string, error) {
	var list []string
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			list = append(list, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

func extractURLFromText(text string) string {
	regex := regexp.MustCompile(`(https?://[^\s]+)`)
	match := regex.FindString(text)
	if match != "" {
		return match
	}
	return ""
}

func generateURLs(baseURL string, shells []string, paths []string) []string {
	var urls []string
	for _, path := range paths {
		for _, shell := range shells {
			fullURL := fmt.Sprintf("%s%s%s", baseURL, path, shell)
			urls = append(urls, fullURL)
		}
	}
	return urls
}

func workerPool(urls []string, numWorkers int, proxies []string, bar *progressbar.ProgressBar) {
	var wg sync.WaitGroup
	urlChan := make(chan string, len(urls))

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(urlChan, &wg, proxies, bar)
	}

	for _, url := range urls {
		urlChan <- url
	}
	close(urlChan)
	wg.Wait()
}

func worker(urlChan chan string, wg *sync.WaitGroup, proxies []string, bar *progressbar.ProgressBar) {
	defer wg.Done()

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20, 
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Timeout:   5 * time.Second, 
		Transport: transport,
	}

	for url := range urlChan {
		// Rate limit requests
		err := limiter.Wait(context.Background()) 
		if err != nil {
			log.Printf("Rate limit error: %v", err)
			continue
		}

		finalURL := ensureHTTPS(url)
		resp, err := client.Get(finalURL)

		// Handle SSL error and try again with HTTP
		if err != nil || resp.StatusCode != 200 {
			finalURL = ensureHTTP(url)
			resp, err = client.Get(finalURL)
		}

		if err != nil {
			log.Printf("❌ [Error] Failed to access %s: %v\n", finalURL, err)
			continue
		}

		if resp.StatusCode == 200 {
			bar.Add(1)
			message := fmt.Sprintf("✅ [Success] Scanned: %s | Status: %d\n", finalURL, resp.StatusCode)
			log.Println(message)
			sendToTelegram(message)
		} else {
			log.Printf("❌ [Failed] Scanned: %s | Status: %d\n", finalURL, resp.StatusCode)
		}

		resp.Body.Close()
	}
}

func getProxy(proxies []string) *url.URL {
	randomIndex := rand.Intn(len(proxies)) // Choose a random proxy
	proxyURL, err := url.Parse(proxies[randomIndex])
	if err != nil {
		log.Fatalf("Failed to parse proxy URL: %v", err)
	}
	return proxyURL
}

func ensureHTTPS(url string) string {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "https://" + url
	}
	return url
}

func ensureHTTP(url string) string {
	if strings.HasPrefix(url, "https://") {
		return "http://" + strings.TrimPrefix(url, "https://")
	} else if !strings.HasPrefix(url, "http://") {
		return "http://" + url
	}
	return url
}

func sendToTelegram(message string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)

	payload := map[string]string{
		"chat_id": chatID,
		"text":    message,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Fatalf("Failed to marshal Telegram payload: %v", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Fatalf("Failed to send message to Telegram: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Failed to send message to Telegram, response code: %d", resp.StatusCode)
	} else {
		log.Printf("Message sent to Telegram successfully")
	}
}
