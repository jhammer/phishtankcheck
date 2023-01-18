package main

import (
	"compress/bzip2"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type phish struct {
	URL string `json:"url"`
}

type database struct {
	username       string
	apiKey         string
	lastUpdated    time.Time
	eTag           string
	urls           map[string]struct{}
	mutex          sync.RWMutex
	searchCount    int64
	searchURLCount int64
}

func (d *database) newRequest(method string) (*http.Request, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://data.phishtank.com/data/%s/online-valid.json.bz2", d.apiKey), nil)

	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "phishtank/"+d.username)
	return req, nil
}

func (d *database) load() error {
	if d.eTag != "" {
		req, err := d.newRequest(http.MethodHead)

		if err != nil {
			return err
		}

		res, err := http.DefaultClient.Do(req)

		if err != nil {
			return err
		}

		defer res.Body.Close()

		if res.Header.Get("ETag") == d.eTag {
			return nil
		}
	}

	req, err := d.newRequest(http.MethodGet)

	if err != nil {
		return err
	}

	res, err := http.DefaultClient.Do(req)

	if err != nil {
		return err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status fetching %s: %v", req.URL, res.StatusCode)
	}

	var phishes []phish

	err = json.NewDecoder(bzip2.NewReader(res.Body)).Decode(&phishes)

	if err != nil {
		return err
	}

	urls := make(map[string]struct{}, 0)

	for _, phish := range phishes {
		urls[strings.ToLower(phish.URL)] = struct{}{}
	}

	d.eTag = res.Header.Get("ETag")
	d.mutex.Lock()
	d.lastUpdated = time.Now()
	d.urls = urls
	d.mutex.Unlock()

	return nil
}

func (d *database) search(urls []string) []string {
	atomic.AddInt64(&d.searchCount, 1)
	atomic.AddInt64(&d.searchURLCount, int64(len(urls)))

	d.mutex.RLock()
	defer d.mutex.RUnlock()

	found := make([]string, 0)

	for _, url := range urls {
		_, present := d.urls[strings.ToLower(url)]

		if present {
			found = append(found, url)
		}
	}

	return found
}

func newDatabase(username string, apiKey string) *database {
	return &database{
		username: username,
		apiKey:   apiKey,
	}
}

func main() {
	portPtr := flag.String("port", "", "port to listen on")
	refreshHoursPtr := flag.Int("refresh", 1, "refresh interval in hours")
	usernamePtr := flag.String("username", "", "Phishtank username")
	apiKeyPtr := flag.String("apiKey", "", "Phishtank API key")

	flag.Parse()

	if *portPtr == "" {
		fmt.Fprintln(os.Stderr, "Port number required")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *usernamePtr == "" || *apiKeyPtr == "" {
		fmt.Fprintln(os.Stderr, "Phishtank username and API key required")
		flag.PrintDefaults()
		os.Exit(1)
	}

	logger, err := syslog.Dial("", "", syslog.LOG_INFO|syslog.LOG_DAEMON, "")

	if err != nil {
		log.Fatal(err)
	}

	db := newDatabase(*usernamePtr, *apiKeyPtr)
	err = db.load()

	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	ticker := time.NewTicker(time.Duration(*refreshHoursPtr) * time.Hour)
	go func() {
		for {
			<-ticker.C

			err := db.load()

			if err != nil {
				logger.Err(fmt.Sprintf("Error refreshing database: %v", err))
			} else {
				logger.Info("Refreshed database")
			}
		}
	}()

	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "", http.StatusMethodNotAllowed)
			return
		}

		var urls []string

		err := json.NewDecoder(r.Body).Decode(&urls)

		if err != nil {
			http.Error(w, "Error decoding body", http.StatusBadRequest)
			return
		}

		phish := db.search(urls)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(phish)
	})
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		db.mutex.RLock()
		defer db.mutex.RUnlock()
		status := struct {
			LastUpdated    time.Time
			EntryCount     int
			SearchCount    int64
			SearchURLCount int64
		}{
			LastUpdated:    db.lastUpdated,
			EntryCount:     len(db.urls),
			SearchCount:    atomic.LoadInt64(&db.searchCount),
			SearchURLCount: atomic.LoadInt64(&db.searchURLCount),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	log.Print("Listening on " + *portPtr)
	log.Fatal(http.ListenAndServe(":"+*portPtr, nil))
}
