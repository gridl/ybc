// CDN booster
//
// This is a dumb HTTP proxy, which caches files obtained from upstreamHost.
//
// Currently go-cdn-booster has the following limitations:
//   * Supports only GET requests.
//   * Doesn't respect HTTP headers received from both the client and
//     the upstream host.
//   * Optimized for small static files aka images, js and css with sizes
//     not exceeding few Mb each.
//   * It caches all files without expiration time.
//     Actually this is a feature :)
//
// Thanks to YBC it has the following features:
//   * Should be extremely fast.
//   * Cached items survive CDN booster restart if backed by cacheFilesPath.
//   * Cache size isn't limited by RAM size.
//   * Optimized for SSDs and HDDs.
//   * Performance shouldn't depend on the number of cached items.
//   * It is deadly simple in configuration and maintenance.
//
package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/ybc/bindings/go/ybc"
	"github.com/vharitonsky/iniflags"
)

var (
	cacheFilesPath = flag.String("cacheFilesPath", "",
		"Path to cache file. Leave empty for anonymous non-persistent cache.\n"+
			"Enumerate multiple files delimited by comma for creating a cluster of caches.\n"+
			"This can increase performance only if frequently accessed items don't fit RAM\n"+
			"and each cache file is located on a distinct physical storage.")
	cacheSize            = flag.Int("cacheSize", 100, "The total cache size in Mbytes")
	goMaxProcs           = flag.Int("goMaxProcs", runtime.NumCPU(), "Maximum number of simultaneous Go threads")
	httpsCertFile        = flag.String("httpsCertFile", "/etc/ssl/certs/ssl-cert-snakeoil.pem", "Path to HTTPS server certificate. Used only if listenHttpsAddr is set")
	httpsKeyFile         = flag.String("httpsKeyFile", "/etc/ssl/private/ssl-cert-snakeoil.key", "Path to HTTPS server key. Used only if listenHttpsAddr is set")
	httpsListenAddrs     = flag.String("httpsListenAddrs", "", "A list of TCP addresses to listen to HTTPS requests. Leave empty if you don't need https")
	listenAddrs          = flag.String("listenAddrs", ":8098", "A list of TCP addresses to listen to HTTP requests. Leave empty if you don't need http")
	maxIdleUpstreamConns = flag.Int("maxIdleUpstreamConns", 50, "The maximum idle connections to upstream host")
	maxItemsCount        = flag.Int("maxItemsCount", 100*1000, "The maximum number of items in the cache")
	readBufferSize       = flag.Int("readBufferSize", 1024, "The size of read buffer for incoming connections")
	statsRequestPath     = flag.String("statsRequestPath", "/static_proxy_stats", "Path to page with statistics")
	upstreamHost         = flag.String("upstreamHost", "www.google.com", "Upstream host to proxy data from. May include port in the form 'host:port'")
	upstreamProtocol     = flag.String("upstreamProtocol", "http", "Use this protocol when talking to the upstream")
	useClientRequestHost = flag.Bool("useClientRequestHost", false, "If set to true, then use 'Host' header from client requests in requests to upstream host. Otherwise use upstreamHost as a 'Host' header in upstream requests")
	writeBufferSize      = flag.Int("writeBufferSize", 4096, "The size of write buffer for incoming connections")
)

var (
	ifNoneMatchResponseHeader         = []byte("HTTP/1.1 304 Not Modified\r\nServer: go-cdn-booster\r\nEtag: W/\"CacheForever\"\r\n\r\n")
	internalServerErrorResponseHeader = []byte("HTTP/1.1 500 Internal Server Error\r\nServer: go-cdn-booster\r\n\r\n")
	notAllowedResponseHeader          = []byte("HTTP/1.1 405 Method Not Allowed\r\nServer: go-cdn-booster\r\n\r\n")
	okResponseHeader                  = []byte("HTTP/1.1 200 OK\r\nServer: go-cdn-booster\r\nCache-Control: public, max-age=31536000\r\nETag: W/\"CacheForever\"\r\n")
	serviceUnavailableResponseHeader  = []byte("HTTP/1.1 503 Service Unavailable\r\nServer: go-cdn-booster\r\n\r\n")
	statsResponseHeader               = []byte("HTTP/1.1 200 OK\r\nServer: go-cdn-booster\r\nContent-Type: text/plain\r\n\r\n")
)

var (
	cache          ybc.Cacher
	stats          Stats
	upstreamClient http.Client
)

func main() {
	iniflags.Parse()

	upstreamHostBytes = []byte(*upstreamHost)

	runtime.GOMAXPROCS(*goMaxProcs)

	cache = createCache()
	defer cache.Close()

	upstreamClient = http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: *maxIdleUpstreamConns,
		},
	}

	var addr string
	for _, addr = range strings.Split(*httpsListenAddrs, ",") {
		go serveHttps(addr)
	}
	for _, addr = range strings.Split(*listenAddrs, ",") {
		go serveHttp(addr)
	}

	waitForeverCh := make(chan int)
	<-waitForeverCh
}

func createCache() ybc.Cacher {
	config := ybc.Config{
		MaxItemsCount: ybc.SizeT(*maxItemsCount),
		DataFileSize:  ybc.SizeT(*cacheSize) * ybc.SizeT(1024*1024),
	}

	var err error
	var cache ybc.Cacher

	cacheFilesPath_ := strings.Split(*cacheFilesPath, ",")
	cacheFilesCount := len(cacheFilesPath_)
	logMessage("Opening data files. This can take a while for the first time if files are big")
	if cacheFilesCount < 2 {
		if cacheFilesPath_[0] != "" {
			config.DataFile = cacheFilesPath_[0] + ".cdn-booster.data"
			config.IndexFile = cacheFilesPath_[0] + ".cdn-booster.index"
		}
		cache, err = config.OpenCache(true)
		if err != nil {
			logFatal("Cannot open cache: [%s]", err)
		}
	} else if cacheFilesCount > 1 {
		config.MaxItemsCount /= ybc.SizeT(cacheFilesCount)
		config.DataFileSize /= ybc.SizeT(cacheFilesCount)
		var configs ybc.ClusterConfig
		configs = make([]*ybc.Config, cacheFilesCount)
		for i := 0; i < cacheFilesCount; i++ {
			cfg := config
			cfg.DataFile = cacheFilesPath_[i] + ".cdn-booster.data"
			cfg.IndexFile = cacheFilesPath_[i] + ".cdn-booster.index"
			configs[i] = &cfg
		}
		cache, err = configs.OpenCluster(true)
		if err != nil {
			logFatal("Cannot open cache cluster: [%s]", err)
		}
	}
	logMessage("Data files have been opened")
	return cache
}

func serveHttps(addr string) {
	if addr == "" {
		return
	}
	cert, err := tls.LoadX509KeyPair(*httpsCertFile, *httpsKeyFile)
	if err != nil {
		logFatal("Cannot load certificate: [%s]", err)
	}
	c := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	ln := tls.NewListener(listen(addr), c)
	logMessage("Listening https on [%s]", addr)
	serve(ln)
}

func serveHttp(addr string) {
	if addr == "" {
		return
	}
	ln := listen(addr)
	logMessage("Listening http on [%s]", addr)
	serve(ln)
}

func listen(addr string) net.Listener {
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		logFatal("Cannot listen [%s]: [%s]", addr, err)
	}
	return ln
}

func serve(ln net.Listener) {
	s := &fasthttp.Server{
		Handler: requestHandler,
		Name:    "go-cdn-booster",
	}
	s.Serve(ln)
}

var keyPool sync.Pool

func requestHandler(ctx *fasthttp.ServerCtx) {
	h := &ctx.Request.Header
	if !h.IsMethodGet() {
		ctx.Error("Method not allowed", 405)
		return
	}

	if fasthttp.EqualBytesStr(h.RequestURI, *statsRequestPath) {
		var w bytes.Buffer
		stats.WriteToStream(&w)
		ctx.Success("text/plain", w.Bytes())
		return
	}

	if len(h.Peek("If-None-Match")) > 0 {
		ctx.Response.Header.StatusCode = 304
		ctx.Response.Header.Set("Etag", "W/\"CacheForever\"")
		atomic.AddInt64(&stats.IfNoneMatchHitsCount, 1)
		return
	}

	v := keyPool.Get()
	if v == nil {
		v = make([]byte, 128)
	}
	key := v.([]byte)
	key = append(key[:0], getRequestHost(h)...)
	key = append(key, h.RequestURI...)
	item, err := cache.GetDeItem(key, time.Second)
	if err != nil {
		if err != ybc.ErrCacheMiss {
			logFatal("Unexpected error when obtaining cache value by key=[%s]: [%s]", key, err)
		}

		atomic.AddInt64(&stats.CacheMissesCount, 1)
		item = fetchFromUpstream(h, key)
		if item == nil {
			ctx.Error("Service unavailable", 503)
			return
		}
	} else {
		atomic.AddInt64(&stats.CacheHitsCount, 1)
	}
	defer item.Close()
	keyPool.Put(v)

	contentType, err := loadContentType(h, item)
	if err != nil {
		ctx.Error("Internal Server Error", 500)
		return
	}

	ctx.Response.Header.Set("Etag", "W/\"CacheForever\"")
	ctx.Response.Header.Set("Cache-Control", "public, max-age=31536000")
	ctx.Response.Header.Set("Content-Type", contentType)
	buf := item.Value()
	buf = buf[len(buf)-item.Available():]
	ctx.Response.Body = append(ctx.Response.Body[:0], buf...)
	atomic.AddInt64(&stats.BytesSentToClients, int64(len(ctx.Response.Body)))
}

func fetchFromUpstream(h *fasthttp.RequestHeader, key []byte) *ybc.Item {
	upstreamUrl := fmt.Sprintf("%s://%s%s", *upstreamProtocol, *upstreamHost, h.RequestURI)
	upstreamReq, err := http.NewRequest("GET", upstreamUrl, nil)
	if err != nil {
		logRequestError(h, "Cannot create request structure for [%s]: [%s]", key, err)
		return nil
	}
	upstreamReq.Host = string(getRequestHost(h))
	resp, err := upstreamClient.Do(upstreamReq)
	if err != nil {
		logRequestError(h, "Cannot make request for [%s]: [%s]", key, err)
		return nil
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logRequestError(h, "Cannot read response for [%s]: [%s]", key, err)
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		logRequestError(h, "Unexpected status code=%d for the response [%s]", resp.StatusCode, key)
		return nil
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentLength := len(body)
	itemSize := contentLength + len(contentType) + 1
	txn, err := cache.NewSetTxn(key, itemSize, ybc.MaxTtl)
	if err != nil {
		logRequestError(h, "Cannot start set txn for response [%s], itemSize=%d: [%s]", key, itemSize, err)
		return nil
	}

	if err = storeContentType(h, txn, contentType); err != nil {
		txn.Rollback()
		return nil
	}

	n, err := txn.Write(body)
	if err != nil {
		logRequestError(h, "Cannot read response [%s] body with size=%d to cache: [%s]", key, contentLength, err)
		txn.Rollback()
		return nil
	}
	if n != contentLength {
		logRequestError(h, "Unexpected number of bytes copied=%d from response [%s] to cache. Expected %d", n, key, contentLength)
		txn.Rollback()
		return nil
	}
	item, err := txn.CommitItem()
	if err != nil {
		logRequestError(h, "Cannot commit set txn for response [%s], size=%d: [%s]", key, contentLength, err)
		return nil
	}
	atomic.AddInt64(&stats.BytesReadFromUpstream, int64(n))
	return item
}

func storeContentType(h *fasthttp.RequestHeader, w io.Writer, contentType string) (err error) {
	strBuf := []byte(contentType)
	strSize := len(strBuf)
	if strSize > 255 {
		logRequestError(h, "Too long content-type=[%s]. Its' length=%d should fit one byte", contentType, strSize)
		err = fmt.Errorf("Too long content-type")
		return
	}
	var sizeBuf [1]byte
	sizeBuf[0] = byte(strSize)
	if _, err = w.Write(sizeBuf[:]); err != nil {
		logRequestError(h, "Cannot store content-type length in cache: [%s]", err)
		return
	}
	if _, err = w.Write(strBuf); err != nil {
		logRequestError(h, "Cannot store content-type string with length=%d in cache: [%s]", strSize, err)
		return
	}
	return
}

func loadContentType(h *fasthttp.RequestHeader, r io.Reader) (contentType string, err error) {
	var sizeBuf [1]byte
	if _, err = r.Read(sizeBuf[:]); err != nil {
		logRequestError(h, "Cannot read content-type length from cache: [%s]", err)
		return
	}
	strSize := int(sizeBuf[0])
	strBuf := make([]byte, strSize)
	if _, err = r.Read(strBuf); err != nil {
		logRequestError(h, "Cannot read content-type string with length=%d from cache: [%s]", strSize, err)
		return
	}
	contentType = string(strBuf)
	return
}

var upstreamHostBytes []byte

func getRequestHost(h *fasthttp.RequestHeader) []byte {
	if *useClientRequestHost {
		return h.Host
	}
	return upstreamHostBytes
}

func logRequestError(h *fasthttp.RequestHeader, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	logMessage("%s - %s - %s. %s", h.RequestURI, h.Referer, h.UserAgent, msg)
}

func logMessage(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s\n", msg)
}

func logFatal(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Fatalf("%s\n", msg)
}

type Stats struct {
	CacheHitsCount        int64
	CacheMissesCount      int64
	IfNoneMatchHitsCount  int64
	BytesReadFromUpstream int64
	BytesSentToClients    int64
}

func (s *Stats) WriteToStream(w io.Writer) {
	fmt.Fprintf(w, "Command-line flags\n")
	flag.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(w, "%s=%v\n", f.Name, f.Value)
	})
	fmt.Fprintf(w, "\n")

	requestsCount := s.CacheHitsCount + s.CacheMissesCount + s.IfNoneMatchHitsCount
	var cacheHitRatio float64
	if requestsCount > 0 {
		cacheHitRatio = float64(s.CacheHitsCount+s.IfNoneMatchHitsCount) / float64(requestsCount) * 100.0
	}
	fmt.Fprintf(w, "Requests count: %d\n", requestsCount)
	fmt.Fprintf(w, "Cache hit ratio: %.3f%%\n", cacheHitRatio)
	fmt.Fprintf(w, "Cache hits: %d\n", s.CacheHitsCount)
	fmt.Fprintf(w, "Cache misses: %d\n", s.CacheMissesCount)
	fmt.Fprintf(w, "If-None-Match hits: %d\n", s.IfNoneMatchHitsCount)
	fmt.Fprintf(w, "Read from upstream: %.3f MBytes\n", float64(s.BytesReadFromUpstream)/1000000)
	fmt.Fprintf(w, "Sent to clients: %.3f MBytes\n", float64(s.BytesSentToClients)/1000000)
	fmt.Fprintf(w, "Upstream traffic saved: %.3f MBytes\n", float64(s.BytesSentToClients-s.BytesReadFromUpstream)/1000000)
	fmt.Fprintf(w, "Upstream requests saved: %d\n", s.CacheHitsCount+s.IfNoneMatchHitsCount)
}
