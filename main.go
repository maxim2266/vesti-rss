package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"

	"vesti-rss/internal/app"
	"vesti-rss/internal/xmlutil"

	"github.com/maxim2266/pump"
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

	// run
	return writeXML(pump.Bind(batchReader, convert(maxItems)))
}

// raw news item
type RawNewsItem struct {
	ID                uint64
	Title, Anons, URL string

	DatePub struct {
		Day, Time string
	}
}

// news item
type NewsItem struct {
	id                uint64
	title, text, link string
	ts                time.Time
}

// batch reader (pipeline source)
func batchReader(yield func([]RawNewsItem) error) error {
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

	// response receiver
	var batch struct {
		Success bool
		Data    []RawNewsItem

		Pagination struct {
			Next string
		}
	}

	// first page URL
	batch.Pagination.Next = server + "/api/news"

	// batch reader loop
	for {
		app.Info("reading page from " + batch.Pagination.Next)

		// make request
		body, err := getResponse(batch.Pagination.Next, client)

		if err != nil {
			return err
		}

		// de-serialise response body
		if err = json.Unmarshal(body, &batch); err != nil {
			return failure("invalid response", err)
		}

		// validate the response
		if !batch.Success {
			return errors.New("response indicates an error")
		}

		if len(batch.Data) == 0 {
			return errors.New("response contains no news")
		}

		// next page URL
		if batch.Pagination.Next, err = makeURL(batch.Pagination.Next); err != nil {
			return failure("next page URL", err)
		}

		// pump
		if err = yield(batch.Data); err != nil {
			return err
		}

		// cleanup
		batch.Success = false
		batch.Data = batch.Data[:0]
	}
}

// converter (pipeline stage)
func convert(maxItems int) pump.S[[]RawNewsItem, *NewsItem] {
	return func(src pump.G[[]RawNewsItem], yield func(*NewsItem) error) error {
		// a set to detect duplicates and count items
		seen := make(map[uint64]struct{}, maxItems+20)

		return src(func(batch []RawNewsItem) (err error) {
			for _, item := range batch {
				// check for duplicate
				if _, yes := seen[item.ID]; yes {
					app.Warn("skipped a duplicate of the news item %d", item.ID)
					continue
				}

				// news item
				news := NewsItem{
					id:    item.ID,
					title: item.Title,
					text:  item.Anons,
				}

				// make link
				if news.link, err = makeURL(item.URL); err != nil {
					app.Warn("skipped news item %d: %s", item.ID, err)
					continue
				}

				// make timestamp
				if news.ts, err = makeTS(item.DatePub.Day, item.DatePub.Time); err != nil {
					app.Warn("skipped news item %d: %s", item.ID, err)
					continue
				}

				seen[item.ID] = struct{}{}

				// pump
				if err = yield(&news); err != nil {
					return
				}
			}

			// check if we've got enough news
			if len(seen) >= maxItems {
				app.Info("processed %d news items.", len(seen))
				err = io.EOF
			}

			return
		})
	}
}

// RSS XML writer
func writeXML(src pump.G[*NewsItem]) error {
	// XML header
	if err := writeString(xmlHeader, time.Now().Year()); err != nil {
		return err
	}

	// buffer
	buff := make([]byte, 0, 8*1024)

	// news items
	err := src(func(news *NewsItem) error {
		buff = buff[:0]

		// open item tag
		buff = append(buff, "<item>"...)

		// title
		buff = xmlutil.AppendTag(buff, "title", news.title)

		// description
		buff = xmlutil.AppendTag(buff, "description", news.text)

		// link
		buff = xmlutil.AppendTag(buff, "link", news.link)

		// GUID
		buff = append(strconv.AppendUint(append(buff, `<guid isPermaLink="false">`...), news.id, 10), "</guid>"...)

		// timestamp
		buff = append(news.ts.AppendFormat(append(buff, "<pubDate>"...), time.RFC1123Z), "</pubDate>"...)

		// close item tag
		buff = append(buff, "</item>\n"...)

		// write
		return write(buff)
	})

	if err != io.EOF {
		return err
	}

	// XML footer
	return writeString("</channel>\n</rss>\n")
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
  <title>Новости</title>
  <link>https://www.vesti.ru/news</link>
  <description>Новости дня от Вести.Ru, интервью, репортажи, фото и видео, новости Москвы и регионов России, новости экономики, погода</description>
  <copyright>© %d Сетевое издание &quot;Вести.Ру&quot;</copyright>
  <image>
    <link>https://www.vesti.ru/news</link>
    <title>Новости</title>
    <url>https://www.vesti.ru/i/logo_fb.png</url>
  </image>
`

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

// output writers
func write(data []byte) (err error) {
	if _, err = os.Stdout.Write(data); err != nil {
		err = failure("writing to STDOUT", err)
	}

	return
}

func writeString(data string, args ...any) (err error) {
	if len(args) > 0 {
		_, err = fmt.Printf(data, args...)
	} else {
		_, err = os.Stdout.WriteString(data)
	}

	if err != nil {
		err = failure("writing to STDOUT", err)
	}

	return
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
