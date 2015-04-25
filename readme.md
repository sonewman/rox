# Rox
Simple configurable HTTP Reverse Proxy inspired by the [net/http/httputil/reverseproxy](https://github.com/golang/go/blob/master/src/net/http/httputil/reverseproxy.go) standard library.

# Usage
### func New
```go
func New(u *url.URL) *Rox
```

Used in its most simplest form:
```go
import (
  "net/http"
  "net/url"
  "rox"
)

func main() {
  u, _ := url.Parse("http://0.0.0.0:9898")
  handler := rox.New(u)
  log.Fatal(http.ListenAndServe(":8080", handler))
}
```
Further documentation on proxy hooks coming soon!

# Licence
MIT
