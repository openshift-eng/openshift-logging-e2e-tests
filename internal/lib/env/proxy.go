package env

import "os"

// Proxy returns the proxy defined by env vars
func Proxy() string {
	var proxy string
	if os.Getenv("http_proxy") != "" {
		proxy = os.Getenv("http_proxy")
	} else if os.Getenv("https_proxy") != "" {
		proxy = os.Getenv("https_proxy")
	}
	return proxy
}
