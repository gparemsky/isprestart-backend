package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type PingData struct {
	Uxtimesec  int `json:"uxtimesec"`
	Cloudflare int `json:"cloudflare"`
	Google     int `json:"google"`
	Facebook   int `json:"facebook"`
	X          int `json:"x"`
}

type PingRequest struct {
	Rows int `json:"rows"`
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	resp := &Response{
		Message: "Hello from Go!",
		Data:    map[string]string{"name": "213234", "age": "30"},
	}
	json.NewEncoder(w).Encode(resp)
}

func main() {
	http.HandleFunc("/hello", helloHandler)

	fmt.Println("Server is listening on port 8081")
	http.ListenAndServe(":8081", nil)
}
