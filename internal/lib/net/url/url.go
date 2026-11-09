package url

import (
	"net/url"
	"path"
)

// Build concats a url `http://foo/bar` with a path `/buzz`.
func Build(u, p, q string) (string, error) {
	url, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	url.Path = path.Join(url.Path, p)
	url.RawQuery = q
	return url.String(), nil
}
