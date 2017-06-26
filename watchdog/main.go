// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alexellis/faas/watchdog/types"
)

func buildFunctionInput(config *WatchdogConfig, r *http.Request) ([]byte, error) {
	var res []byte
	var requestBytes []byte
	var err error

	if r.Body != nil {
		defer r.Body.Close()
	}

	requestBytes, _ = ioutil.ReadAll(r.Body)
	if config.marshalRequest {
		marshalRes, marshalErr := types.MarshalRequest(requestBytes, &r.Header)
		err = marshalErr
		res = marshalRes
	} else {
		res = requestBytes
	}
	return res, err
}

func debugHeaders(source *http.Header, direction string) {
	for k, vv := range *source {
		fmt.Printf("[%s] %s=%s\n", direction, k, vv)
	}
}

func pipeRequest(config *WatchdogConfig, w http.ResponseWriter, r *http.Request, method string, hasBody bool) {
	startTime := time.Now()

	parts := strings.Split(config.faasProcess, " ")

	if config.debugHeaders {
		debugHeaders(&r.Header, "in")
	}

	targetCmd := exec.Command(parts[0], parts[1:]...)

	envs := getAdditionalEnvs(config, r, method)
	if len(envs) > 0 {
		targetCmd.Env = envs

	}

	writer, _ := targetCmd.StdinPipe()

	var out []byte
	var err error
	var requestBody []byte

	var wg sync.WaitGroup

	wgCount := 2
	if hasBody == false {
		wgCount = 1
	}

	if hasBody {
		var buildInputErr error
		requestBody, buildInputErr = buildFunctionInput(config, r)
		if buildInputErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(buildInputErr.Error()))
			return
		}
	}

	wg.Add(wgCount)

	// Only write body if this is appropriate for the method.
	if hasBody {
		// Write to pipe in separate go-routine to prevent blocking
		go func() {
			defer wg.Done()
			writer.Write(requestBody)
			writer.Close()
		}()
	}

	go func() {
		defer wg.Done()
		out, err = targetCmd.CombinedOutput()
	}()

	wg.Wait()

	if err != nil {
		if config.writeDebug == true {
			log.Println(targetCmd, err)
		}
		w.WriteHeader(http.StatusInternalServerError)
		response := bytes.NewBufferString(err.Error())
		w.Write(response.Bytes())
		return
	}
	if config.writeDebug == true {
		os.Stdout.Write(out)
	}

	if len(config.contentType) > 0 {
		w.Header().Set("Content-Type", config.contentType)
	} else {

		// Match content-type of caller if no override specified.
		clientContentType := r.Header.Get("Content-Type")
		if len(clientContentType) > 0 {
			w.Header().Set("Content-Type", clientContentType)
		}
	}

	execTime := time.Since(startTime).Seconds()
	w.Header().Set("X-Duration-Seconds", fmt.Sprintf("%f", execTime))

	w.WriteHeader(200)
	w.Write(out)

	if config.debugHeaders {
		header := w.Header()
		debugHeaders(&header, "out")
	}
}

func getAdditionalEnvs(config *WatchdogConfig, r *http.Request, method string) []string {
	var envs []string

	if config.cgiHeaders {
		envs = os.Environ()
		for k, v := range r.Header {
			kv := fmt.Sprintf("Http_%s=%s", k, v[0])
			envs = append(envs, kv)
		}
		envs = append(envs, fmt.Sprintf("Http_Method=%s", method))

		if len(r.URL.RawQuery) > 0 {
			envs = append(envs, fmt.Sprintf("Http_Query=%s", r.URL.RawQuery))
		}
	}

	return envs
}

func makeRequestHandler(config *WatchdogConfig) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case
			"POST",
			"PUT",
			"DELETE",
			"UPDATE":
			pipeRequest(config, w, r, r.Method, true)
			break
		case
			"GET":
			pipeRequest(config, w, r, r.Method, false)
			break
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)

		}
	}
}

func main() {
	osEnv := types.OsEnv{}
	readConfig := ReadConfig{}
	config := readConfig.Read(osEnv)

	if len(config.faasProcess) == 0 {
		log.Panicln("Provide a valid process via fprocess environmental variable.")
		return
	}

	readTimeout := time.Duration(config.readTimeout) * time.Second
	writeTimeout := time.Duration(config.writeTimeout) * time.Second

	s := &http.Server{
		Addr:           ":8080",
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
		MaxHeaderBytes: 1 << 20, // Max header of 1MB
	}

	http.HandleFunc("/", makeRequestHandler(&config))

	if config.suppressLock == false {
		path := "/tmp/.lock"
		log.Printf("Writing lock-file to: %s\n", path)
		writeErr := ioutil.WriteFile(path, []byte{}, 0660)
		if writeErr != nil {
			log.Panicf("Cannot write %s. To disable lock-file set env suppress_lock=true.\n Error: %s.\n", path, writeErr.Error())
		}
	}
	log.Fatal(s.ListenAndServe())
}
