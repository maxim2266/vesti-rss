package main

import (
	"bytes"
	"encoding/json"
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
		numItems int
		logLevel string
	)

	flag.IntVar(&numItems, "num-items", 100, "number of news items to fetch, from 1 to 500; the actual number will be rounded up to the page size")
	flag.StringVar(&logLevel, "log-level", "error", "logging level, one of: trace, info, warning, error")

	flag.Parse()

	// validate and apply flags
	if err = app.SetLogLevel(logLevel); err != nil {
		return
	}

	if numItems < 1 || numItems > 500 {
		return errors.New("invalid number of items: " + strconv.Itoa(numItems))
	}

	// XML header
	if err = writeString(xmlHeader); err != nil {
		return
	}

	// buffer
	buff := append(make([]byte, 0, 4*1024), xmlPrefix...)

	// pipeline
	it := pump.Iter(pump.Bind(source(numItems), converter))

	// read the news and write out XML
	for news := range it.All {
		// title
		buff = append(xmlutil.AppendEscaped(buff[:xmlPrefixLen], news.title), "</title><description>"...)

		// description
		buff = append(xmlutil.AppendEscaped(buff, news.text), "</description><link>"...)

		// link
		buff = append(xmlutil.AppendEscaped(buff, news.link), `</link><guid isPermaLink="false">`...)

		// GUID
		buff = append(strconv.AppendUint(buff, news.id, 10), "</guid><pubDate>"...)

		// timestamp
		buff = append(news.ts.AppendFormat(buff, time.RFC1123Z), "</pubDate></item>\n"...)

		// write
		if err = write(buff); err != nil {
			return
		}
	}

	if it.Err != nil {
		return it.Err
	}

	// XML footer
	return writeString("</channel>\n</rss>\n")
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

// news reader source (generator constructor)
func source(numItems int) pump.Gen[*RawNewsItem] {
	return func(yield func(*RawNewsItem) error) error {
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

		// a set to detect duplicates and count items
		seen := make(map[uint64]struct{}, numItems+20)

		// batch reader loop
		for {
			app.Info("reading page from " + batch.Pagination.Next)

			// make request
			body, err := getResponse(batch.Pagination.Next, client)

			if err != nil {
				return err
			}

			// de-serialise response body
			batch.Success = false
			batch.Data = batch.Data[:0]
			batch.Pagination.Next = ""

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

			// loop over the news batch
			for i := range batch.Data {
				item := &batch.Data[i]

				// check for duplicate
				if _, yes := seen[item.ID]; yes {
					app.Warn("skipped a duplicate of the news item %d", item.ID)
					continue
				}

				// yield
				if err = yield(item); err != nil {
					return err
				}

				// mark as seen
				seen[item.ID] = struct{}{}
			}

			// check if we've got enough news
			if len(seen) >= numItems {
				app.Info("processed %d news items.", len(seen))
				return nil
			}
		}
	}
}

// converter (a pipeline stage)
func converter(src pump.Gen[*RawNewsItem], yield func(*NewsItem) error) error {
	return src(func(item *RawNewsItem) error {
		// news item
		news := NewsItem{
			id:    item.ID,
			title: item.Title,
			text:  item.Anons,
		}

		// make link
		var err error

		if news.link, err = makeURL(item.URL); err != nil {
			app.Warn("skipped news item %d: %s", item.ID, err)
			return nil // skip
		}

		// make timestamp
		if news.ts, err = makeTS(item.DatePub.Day, item.DatePub.Time); err != nil {
			app.Warn("skipped news item %d: %s", item.ID, err)
			return nil // skip
		}

		return yield(&news)
	})
}

var xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
  <title>Новости</title>
  <link>https://www.vesti.ru/news</link>
  <description>Новости дня от Вести.Ru, интервью, репортажи, фото и видео, новости Москвы и регионов России, новости экономики, погода</description>
  <copyright>© ` + strconv.Itoa(time.Now().Year()) + ` Сетевое издание &quot;Вести.Ру&quot;</copyright>
  <image>
    <link>https://www.vesti.ru/news</link>
    <title>Новости</title>
    <url>https://www.vesti.ru/i/logo_fb.png</url>
  </image>
`

const (
	xmlPrefix    = "<item><title>"
	xmlPrefixLen = len(xmlPrefix)
)

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

	// construct timestamp object
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

// output writers
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

// compose error message from a prefix and an error
func failure(prefix string, err error) error {
	return errors.New(prefix + ": " + err.Error())
}
