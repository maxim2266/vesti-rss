package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"

	"vesti-rss/internal/app"
	"vesti-rss/internal/xmlutil"
)

// news server
const (
	server  = "https://www.vesti.ru"
	version = "0.1"
)

// entry point
func main() {
	os.Stdin.Close()
	app.Run(theApp)
}

// the application
func theApp() (err error) {
	// load MSK location
	if msk, err = time.LoadLocation("Europe/Moscow"); err != nil {
		return
	}

	// read flags
	var (
		maxItems int
		logLevel string
	)

	flag.IntVar(&maxItems, "max-items", 100, "number of news items to fetch, any value from 1 to 500")
	flag.StringVar(&logLevel, "log-level", "error", "logging level, one of: trace, info, warning, error")

	flag.Parse()

	// validate and apply flags
	if err = app.SetLogLevel(logLevel); err != nil {
		return
	}

	if maxItems < 1 || maxItems > 500 {
		return errors.New("invalid number of items: " + strconv.Itoa(maxItems))
	}

	// start news reader
	newsChan := startReader(maxItems)

	// XML header
	if err = writeString(xmlHeader); err != nil {
		return
	}

	// a set to detect duplicate items
	seen := make(map[uint64]struct{}, maxItems+20)

	// buffer
	buff := make([]byte, 0, 8*1024)

	// news reader loop
	for batch := range newsChan {
		for _, item := range batch {
			// check for duplicate
			if _, yes := seen[item.ID]; yes {
				app.Warn("skipped a duplicate of the news item %d", item.ID)
				continue
			}

			// check link
			var link string

			if link, err = makeURL(item.URL); err != nil {
				app.Warn("skipped news item with id %d: %s", item.ID, err)
				continue
			}

			// check timestamp
			var ts time.Time

			if ts, err = makeTS(item.DatePub.Day, item.DatePub.Time); err != nil {
				app.Warn("skipped news item with id %d: %s", item.ID, err)
				continue
			}

			// serialise the item
			buff = append(buff, "<item>"...)

			// title
			buff = xmlutil.AppendTag(buff, "title", item.Title)

			// description
			buff = xmlutil.AppendTag(buff, "description", item.Anons)

			// link
			buff = xmlutil.AppendTag(buff, "link", link)

			// GUID
			buff = append(strconv.AppendUint(append(buff, `<guid isPermaLink="false">`...), item.ID, 10), "</guid>"...)

			// timestamp
			buff = append(ts.AppendFormat(append(buff, "<pubDate>"...), time.RFC1123Z), "</pubDate>"...)

			// item complete
			buff = append(buff, "</item>\n"...)
			seen[item.ID] = struct{}{}
		}

		// write out
		if err = write(buff); err != nil {
			return
		}

		buff = buff[:0]
	}

	// XML footer
	if err = writeString("</channel>\n</rss>\n"); err != nil {
		return
	}

	app.Info("processed %d news items.", len(seen))
	return
}

const xmlHeader = xml.Header + `<rss version="2.0">
<channel>
  <title>vesti.ru Новости</title>
  <link>` + server + `</link>
  <description>Лента новостей</description>
`

func write(data []byte) (err error) {
	if _, err = os.Stdout.Write(data); err != nil {
		err = failure("writing to STDOUT", err)
	}

	return
}

func writeString(data string) (err error) {
	if _, err = os.Stdout.WriteString(data); err != nil {
		err = failure("writing to STDOUT", err)
	}

	return
}

// news item
type NewsItem struct {
	ID                uint64
	Title, Anons, URL string

	DatePub struct {
		Day, Time string
	}
}

// news reader constructor
func startReader(n int) <-chan []NewsItem {
	ch := make(chan []NewsItem, 10)

	app.Go(func() error {
		defer close(ch)

		// batch reader function
		next := newBatchReader()

		// batch reader loop
		for n > 0 {
			// next batch
			batch, err := next()

			if err != nil {
				return err
			}

			n -= len(batch)

			// feed the channel
			select {
			case ch <- batch:
				// ok
			case <-app.Shut():
				return errors.New("batch reader thread stopped due to application shutdown")
			}
		}

		app.Info("news reader thread has completed")
		return nil
	})

	app.Info("news reader thread started")
	return ch
}

// batch reader constructor
func newBatchReader() func() ([]NewsItem, error) {
	// HTTP client
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    1,
			MaxConnsPerHost: 1,
			IdleConnTimeout: 20 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// request URL
	reqURL := server + "/api/news"

	// the next() function
	return func() ([]NewsItem, error) {
		app.Info("reading page from " + reqURL)

		// make request
		body, err := getResponse(reqURL, client)

		if err != nil {
			return nil, err
		}

		// de-serialise response body
		var batch struct {
			Success bool
			Data    []NewsItem

			Pagination struct {
				Next string
			}
		}

		if err = json.Unmarshal(body, &batch); err != nil {
			return nil, failure("invalid response", err)
		}

		// validate the response
		if !batch.Success {
			return nil, errors.New("response indicates an error")
		}

		if len(batch.Data) == 0 {
			return nil, errors.New("response contains no news")
		}

		// next page URL
		if reqURL, err = makeURL(batch.Pagination.Next); err != nil {
			return nil, failure("next page URL", err)
		}

		// all done
		return batch.Data, nil
	}
}

// make HTTP request and return the response body
func getResponse(reqURL string, client *http.Client) ([]byte, error) {
	// HTTP request
	req, err := http.NewRequestWithContext(app.Context(), http.MethodGet, reqURL, nil)

	if err != nil {
		return nil, failure("creating HTTP request", err)
	}

	// HTTP headers
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vesti-rss/"+version)

	// make the request
	resp, err := client.Do(req)

	if err != nil {
		return nil, failure("making HTTP request", err)
	}

	defer resp.Body.Close()

	// check HTTP status
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)

		msg := "HTTP request returned status code " + strconv.Itoa(resp.StatusCode)

		if s := http.StatusText(resp.StatusCode); len(s) > 0 {
			msg += " (" + s + ")"
		}

		return nil, errors.New(msg)
	}

	// strangely enough, their server returns errors in HTML and with HTTP code 200,
	// so here we have to read the whole body to detect such an error before attempting
	// to de-serialise the content
	body, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, failure("reading response", err)
	}

	body = bytes.TrimSpace(body)

	if len(body) == 0 || body[0] != '{' {
		return nil, errors.New("response is either empty, or in a wrong format")
	}

	// all done
	return body, nil
}

// make full URL with the given path, and validate it
func makeURL(s string) (string, error) {
	if len(s) == 0 || s[0] != '/' {
		return "", errors.New("invalid path: " + strconv.Quote(s))
	}

	u, err := url.ParseRequestURI(server + s)

	if err != nil {
		return "", err
	}

	return u.String(), nil
}

// compose timestamp from date and time
func makeTS(d, t string) (time.Time, error) {
	// match date
	m := matchDate(d)

	if len(m) != 4 {
		return time.Time{}, errors.New("invalid date: " + strconv.Quote(d))
	}

	// extract date values
	year, _ := strconv.Atoi(m[3])
	month := monthMap[m[2]]

	if month == 0 {
		return time.Time{}, errors.New("invalid month: " + strconv.Quote(d))
	}

	day, _ := strconv.Atoi(m[1])

	// match time
	if m = matchTime(t); len(m) != 3 {
		return time.Time{}, errors.New("invalid time: " + strconv.Quote(d))
	}

	// extract time values
	hour, _ := strconv.Atoi(m[1])
	minute, _ := strconv.Atoi(m[2])

	// construct timesatmp object
	return time.Date(year, month, day, hour, minute, 0, 0, msk).UTC(), nil
}

var (
	monthMap = map[string]time.Month{
		"января":   time.January,
		"февраля":  time.February,
		"марта":    time.March,
		"апреля":   time.April,
		"мая":      time.May,
		"июня":     time.June,
		"июля":     time.July,
		"августа":  time.August,
		"сентября": time.September,
		"октября":  time.October,
		"ноября":   time.November,
		"декабря":  time.December,
	}

	matchDate = regexp.MustCompile(`^((?:0?[1-9])|(?:[1-2][0-9])|(?:3[01]))[[:blank:]]+(\p{Cyrillic}+)[[:blank:]]+(20[[:digit:]]{2})$`).FindStringSubmatch
	matchTime = regexp.MustCompile(`^((?:[01][0-9])|(?:2[0-3])):([0-5][0-9])$`).FindStringSubmatch

	msk *time.Location
)

// compose error message from a prefix and an error
func failure(prefix string, err error) error {
	return errors.New(prefix + ": " + err.Error())
}
