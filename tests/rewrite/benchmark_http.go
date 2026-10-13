// benchmark_http independently measures real Go HTTP roundtrips with reuse.
// Config arrives over a private stdin pipe; access tokens are never printed.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

type config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}
type sample struct {
	Requests    int     `json:"requests"`
	Concurrency int     `json:"concurrency"`
	P50         float64 `json:"p50_ms"`
	P95         float64 `json:"p95_ms"`
	Max         float64 `json:"max_ms"`
}

func measure(client *http.Client, c config, path string, authenticated bool, perWorker int) (sample, error) {
	var mu sync.Mutex
	var group sync.WaitGroup
	var failure error
	times := make([]float64, 0, 20*perWorker)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range perWorker {
				req, err := http.NewRequest("GET", c.URL+path, nil)
				if err != nil {
					mu.Lock()
					failure = err
					mu.Unlock()
					return
				}
				if authenticated {
					req.Header.Set("Authorization", "Bearer "+c.Token)
				}
				start := time.Now()
				response, err := client.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != 200 {
						err = fmt.Errorf("HTTP status %d", response.StatusCode)
					}
				}
				elapsed := float64(time.Since(start)) / float64(time.Millisecond)
				mu.Lock()
				if err != nil {
					failure = err
					mu.Unlock()
					return
				}
				times = append(times, elapsed)
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if failure != nil {
		return sample{}, failure
	}
	sort.Float64s(times)
	return sample{len(times), 20, times[len(times)/2], times[(len(times)*95/100)-1], times[len(times)-1]}, nil
}
func main() {
	var c config
	if err := json.NewDecoder(os.Stdin).Decode(&c); err != nil {
		panic("Invalid private benchmark configuration")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" {
		panic("Only a local HTTP fixture is supported")
	}
	tr := &http.Transport{MaxConnsPerHost: 20, MaxIdleConns: 20, MaxIdleConnsPerHost: 20, IdleConnTimeout: 30 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	out := map[string]any{"transport": "Go net/http keep-alive;20concurrent;200measured requests per endpoint;100warmup"}
	for _, target := range []struct {
		name, path string
		auth       bool
	}{{"health", "/healthz", false}, {"status", "/api/v1/system/status", true}} {
		if _, err = measure(client, c, target.path, target.auth, 5); err != nil {
			panic("Benchmark warmup failed")
		}
		value, e := measure(client, c, target.path, target.auth, 10)
		if e != nil {
			panic("Real HTTP benchmark failed")
		}
		out[target.name] = value
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
