package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"vesti-rss/internal/app"
	"vesti-rss/internal/xmlutil"
)

func main() {
	os.Stdin.Close()
	app.Run(theApp)
}

func theApp() error {
	// read flags
	var (
		maxItems int
		logLevel string
	)

	flag.IntVar(&maxItems, "max-items", 100, "number of news items to fetch, any value from 20 to 500")
	flag.StringVar(&logLevel, "log-level", "info", "logging level, one of: trace, info, warning, error")

	flag.Parse()

	// validate and apply flags
	if err := app.SetLogLevel(logLevel); err != nil {
		return err
	}

	if maxItems < 20 || maxItems > 500 {
		return errors.New("invalid number of items: " + strconv.Itoa(maxItems))
	}

	// start reader
	newsChan := startReader(maxItems)

	// XML header
	if err := writeString(xmlHeader); err != nil {
		return err
	}

	// news reader loop
	for batch := range newsChan {
		if err := write(batch); err != nil {
			return err
		}
	}

	// XML footer
	return writeString("</channel>\n</rss>\n")
}

const xmlHeader = xml.Header + `<rss version="2.0">
<channel>
  <title>vesti.ru Новости</title>
  <link>https://www.vesti.ru</link>
  <description>Лента новостей</description>
`

func write(data []byte) (err error) {
	if _, err = os.Stdout.Write(data); err != nil {
		err = fmt.Errorf("writing to STDOUT: %w", err)
	}

	return
}

func writeString(data string) (err error) {
	if _, err = os.Stdout.WriteString(data); err != nil {
		err = fmt.Errorf("writing to STDOUT: %w", err)
	}

	return
}

// news reader constructor
func startReader(n int) <-chan []byte {
	ch := make(chan []byte, 10)

	app.Go(func() error {
		defer close(ch)

		var numItems int

		// batch reader function
		next := newBatchReader()

		// batch reader loop
		for numItems < n {
			// next batch
			batch, size, err := next()

			if err != nil {
				return err
			}

			numItems += size

			// feed the channel
			select {
			case ch <- batch:
				// ok
			case <-app.Shut():
				return errors.New("batch reader thread stopped due to application shutdown")
			}
		}

		app.Info("news reader thread has completed; received %d news items", numItems)
		return nil
	})

	app.Info("news reader thread started")
	return ch
}

func newBatchReader() func() ([]byte, int, error) {
	// HTTP client
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    1,
			MaxConnsPerHost: 1,
			IdleConnTimeout: 20 * time.Second,
		},
	}

	// request URL
	const server = "https://www.vesti.ru"
	reqURL := server + "/api/news"

	// the next() function
	return func() ([]byte, int, error) {
		app.Info("reading page from " + reqURL)

		// HTTP request
		req, err := http.NewRequestWithContext(app.Context(), http.MethodGet, reqURL, nil)

		if err != nil {
			return nil, 0, fmt.Errorf("creating HTTP request to %s: %w", reqURL, err)
		}

		// HTTP headers
		// 		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "vesti-rss/0.1")

		// make the request
		resp, err := client.Do(req)

		if err != nil {
			return nil, 0, fmt.Errorf("making HTTP request to %s: %w", reqURL, err)
		}

		defer resp.Body.Close()

		// check HTTP status
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body)

			msg := fmt.Sprintf("HTTP request to %s returned status code %d", reqURL, resp.StatusCode)

			if s := http.StatusText(resp.StatusCode); len(s) > 0 {
				msg += " (" + s + ")"
			}

			return nil, 0, errors.New(msg)
		}

		// strangely enough, their server returns errors in HTML and with HTTP code 200,
		// so here we have to read the whole body to detect such an error before attempting
		// to de-serialise the content
		body, err := io.ReadAll(resp.Body)

		if err != nil {
			return nil, 0, fmt.Errorf("reading response from %s: %w", reqURL, err)
		}

		body = bytes.TrimSpace(body)

		if len(body) == 0 || body[0] != '{' {
			if len(body) > 0 {
				println(string(body))
			}

			return nil, 0, fmt.Errorf("response from %s is either empty, or in a wrong format", reqURL)
		}

		// de-serialise the response body
		var batch struct {
			Success bool

			Pagination struct {
				Next string
			}

			Data []struct {
				ID                uint64
				Title, Anons, URL string

				DatePub struct {
					Day, Time string
				}
			}
		}

		if err = json.Unmarshal(body, &batch); err != nil {
			return nil, 0, fmt.Errorf("invalid response from %s: %w", reqURL, err)
		}

		// validate the response
		if !batch.Success {
			return nil, 0, fmt.Errorf("response from %s indicates an error", reqURL)
		}

		if len(batch.Data) == 0 {
			return nil, 0, fmt.Errorf("response from %s contains no batch", reqURL)
		}

		// compose result
		n := len(batch.Data)
		res := make([]byte, 0, 32*1024)

		for _, item := range batch.Data {
			// start item record
			res = append(res, "<item>"...)

			// title
			res = xmlutil.AppendTag(res, "title", item.Title)

			// description
			res = xmlutil.AppendTag(res, "description", item.Anons)

			// link
			res = xmlutil.AppendTag(res, "link", server+item.URL)

			// GUID
			res = append(strconv.AppendUint(append(res, `<guid isPermaLink="false">`...), item.ID, 10), "</guid>"...)

			// item complete
			res = append(res, "</item>\n"...)
		}

		// next page URL
		reqURL = server + batch.Pagination.Next

		// all done
		return res, n, nil
	}
}
