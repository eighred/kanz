// probe performs bounded HTTP reads from an ordinary pod's network namespace.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "wait" {
		time.Sleep(2 * time.Hour)
		return
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: probe URL tenant-or-empty")
		os.Exit(2)
	}
	req, err := http.NewRequest(http.MethodGet, os.Args[1], nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if os.Args[2] != "" {
		req.Header.Set("X-Kanz-Principal-Tenant", os.Args[2])
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result := struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Error  string `json:"error"`
	}{}
	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
	} else {
		defer resp.Body.Close()
		result.Status = resp.StatusCode
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		result.Body = string(body)
		if readErr != nil {
			result.Error = readErr.Error()
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(2)
	}
}
