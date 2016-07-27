package main

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layeh/gopher-luar"
	"github.com/yuin/gopher-lua"
)

type httpRequestParam struct {
	// HTTP method, such as "GET"
	Method string
	// Request URL without query strings
	Url string
	// Post body data
	Data []byte
	// If Method is "GET", this value is used as query strings, otherwise as a form-encoded value
	Params []string
	// Additional HTTP headers
	Headers []string
}

var httpDefaultHeaders []string = []string{}

func httpRequest(p httpRequestParam) (*http.Response, error) {
	client := &http.Client{Timeout: time.Duration(10) * time.Second}
	values := url.Values{}
	if p.Params != nil {
		for i := 0; i < len(p.Params); i += 2 {
			values.Add(p.Params[i], p.Params[i+1])
		}
	}

	ul := p.Url
	var body io.Reader = strings.NewReader("")
	isform := false
	switch strings.ToUpper(p.Method) {
	case "GET":
		if p.Params != nil {
			ul = ul + "?" + values.Encode()
		}
	default:
		isform = p.Params != nil && p.Data == nil
		if isform {
			body = strings.NewReader(values.Encode())
		} else if p.Data != nil {
			body = bytes.NewReader(p.Data)
		}
	}
	req, err := http.NewRequest(strings.ToUpper(p.Method), ul, body)
	if err != nil {
		return nil, err
	}
	httpDefaultHeaders := []string{}
	for i := 0; i < len(httpDefaultHeaders); i += 2 {
		if req.Header.Get(httpDefaultHeaders[i]) == "" {
			req.Header.Set(httpDefaultHeaders[i], httpDefaultHeaders[i+1])
		}
	}
	if isform {
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	}
	if p.Headers != nil {
		for i := 0; i < len(p.Headers); i += 2 {
			req.Header.Add(p.Headers[i], p.Headers[i+1])
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

var requestsMod map[string]lua.LGFunction = map[string]lua.LGFunction{
	"request": func(L *lua.LState) int {
		opt := L.CheckTable(1)
		param := httpRequestParam{}
		param.Method = "GET"
		if method, ok := getStringField(L, opt, "method"); ok {
			param.Method = method
		}
		if url, ok := getStringField(L, opt, "url"); ok {
			param.Url = url
		}
		if data, ok := getStringField(L, opt, "data"); ok {
			param.Data = ([]byte)(data)
		}
		lv := L.GetField(opt, "params")
		if tbl, ok := lv.(*lua.LTable); ok {
			params := []string{}
			tbl.ForEach(func(k, v lua.LValue) {
				params = append(params, v.String())
			})
			param.Params = params
		}
		lv = L.GetField(opt, "headers")
		if tbl, ok := lv.(*lua.LTable); ok {
			headers := []string{}
			tbl.ForEach(func(k, v lua.LValue) {
				headers = append(headers, v.String())
			})
			param.Headers = headers
		}
		res, err := httpRequest(param)
		if err != nil {
			pushN(L, lua.LNil, lua.LString(err.Error()))
			return 2
		}
		body, err := ioutil.ReadAll(res.Body)
		defer res.Body.Close()
		if err != nil {
			pushN(L, lua.LNil, lua.LString(err.Error()))
			return 2
		}
		pushN(L, lua.LString(string(body)), luar.New(L, res))
		return 2
	},
}
