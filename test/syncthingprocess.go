// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// +build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/syncthing/protocol"
)

var env = []string{
	"HOME=.",
	"STGUIAPIKEY=" + apiKey,
	"STNORESTART=1",
}

type syncthingProcess struct {
	instance  string
	argv      []string
	port      int
	apiKey    string
	csrfToken string
	lastEvent int
	id        protocol.DeviceID

	cmd   *exec.Cmd
	logfd *os.File
}

func (p *syncthingProcess) start() error {
	if p.logfd == nil {
		logfd, err := os.Create("logs/" + getTestName() + "-" + p.instance + ".out")
		if err != nil {
			return err
		}
		p.logfd = logfd
	}

	binary := "../bin/syncthing"

	// We check to see if there's an instance specific binary we should run,
	// for example if we are running integration tests between different
	// versions. If there isn't, we just go with the default.
	if _, err := os.Stat(binary + "-" + p.instance); err == nil {
		binary = binary + "-" + p.instance
	}
	if _, err := os.Stat(binary + "-" + p.instance + ".exe"); err == nil {
		binary = binary + "-" + p.instance + ".exe"
	}

	cmd := exec.Command(binary, p.argv...)
	cmd.Stdout = p.logfd
	cmd.Stderr = p.logfd
	cmd.Env = append(os.Environ(), env...)

	err := cmd.Start()
	if err != nil {
		return err
	}
	p.cmd = cmd

	for {
		time.Sleep(250 * time.Millisecond)

		resp, err := p.get("/rest/system")
		if err != nil {
			continue
		}

		var sysData map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&sysData)
		resp.Body.Close()
		if err != nil {
			// This one is unexpected. Print it.
			log.Println("/rest/system (JSON):", err)
			continue
		}

		id, err := protocol.DeviceIDFromString(sysData["myID"].(string))
		if err != nil {
			// This one is unexpected. Print it.
			log.Println("/rest/system (myID):", err)
			continue
		}

		p.id = id

		return nil
	}
}

func (p *syncthingProcess) stop() error {
	p.cmd.Process.Signal(os.Kill)
	p.cmd.Wait()

	fd, err := os.Open(p.logfd.Name())
	if err != nil {
		return err
	}
	defer fd.Close()

	raceConditionStart := []byte("WARNING: DATA RACE")
	raceConditionSep := []byte("==================")
	sc := bufio.NewScanner(fd)
	race := false
	for sc.Scan() {
		line := sc.Bytes()
		if race {
			fmt.Printf("%s\n", line)
			if bytes.Contains(line, raceConditionSep) {
				race = false
			}
		} else if bytes.Contains(line, raceConditionStart) {
			fmt.Printf("%s\n", raceConditionSep)
			fmt.Printf("%s\n", raceConditionStart)
			race = true
			if err == nil {
				err = errors.New("Race condition detected")
			}
		}
	}
	return err
}

func (p *syncthingProcess) get(path string) (*http.Response, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path), nil)
	if err != nil {
		return nil, err
	}
	if p.apiKey != "" {
		req.Header.Add("X-API-Key", p.apiKey)
	}
	if p.csrfToken != "" {
		req.Header.Add("X-CSRF-Token", p.csrfToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *syncthingProcess) post(path string, data io.Reader) (*http.Response, error) {
	client := &http.Client{
		Timeout: 600 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path), data)
	if err != nil {
		return nil, err
	}
	if p.apiKey != "" {
		req.Header.Add("X-API-Key", p.apiKey)
	}
	if p.csrfToken != "" {
		req.Header.Add("X-CSRF-Token", p.csrfToken)
	}
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *syncthingProcess) peerCompletion() (map[string]int, error) {
	resp, err := p.get("/rest/debug/peerCompletion")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	comp := map[string]int{}
	err = json.NewDecoder(resp.Body).Decode(&comp)

	// Remove ourselves from the set. In the remaining map, all peers should
	// be att 100% if we're in sync.
	for id := range comp {
		if id == p.id.String() {
			delete(comp, id)
		}
	}

	return comp, err
}

func (p *syncthingProcess) allPeersInSync() error {
	comp, err := p.peerCompletion()
	if err != nil {
		return err
	}
	for id, val := range comp {
		if val != 100 {
			return fmt.Errorf("%.7s at %d%%", id, val)
		}
	}
	return nil
}

type model struct {
	GlobalBytes   int
	GlobalDeleted int
	GlobalFiles   int
	InSyncBytes   int
	InSyncFiles   int
	Invalid       string
	LocalBytes    int
	LocalDeleted  int
	LocalFiles    int
	NeedBytes     int
	NeedFiles     int
	State         string
	StateChanged  time.Time
	Version       int
}

func (p *syncthingProcess) model(folder string) (model, error) {
	resp, err := p.get("/rest/model?folder=" + folder)
	if err != nil {
		return model{}, err
	}

	var res model
	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return model{}, err
	}

	return res, nil
}

type event struct {
	ID   int
	Time time.Time
	Type string
	Data interface{}
}

func (p *syncthingProcess) events() ([]event, error) {
	resp, err := p.get(fmt.Sprintf("/rest/events?since=%d", p.lastEvent))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var evs []event
	err = json.NewDecoder(resp.Body).Decode(&evs)
	if err != nil {
		return nil, err
	}
	p.lastEvent = evs[len(evs)-1].ID
	return evs, err
}

type versionResp struct {
	Version string
}

func (p *syncthingProcess) version() (string, error) {
	resp, err := p.get("/rest/version")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var v versionResp
	err = json.NewDecoder(resp.Body).Decode(&v)
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

func (p *syncthingProcess) rescan(folder string) error {
	resp, err := p.post("/rest/scan?folder="+folder, nil)
	if err != nil {
		return err
	}
	data, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Rescan %q: status code %d: %s", folder, resp.StatusCode, data)
	}
	return nil
}

func (p *syncthingProcess) reset(folder string) error {
	resp, err := p.post("/rest/reset?folder="+folder, nil)
	if err != nil {
		return err
	}
	data, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Reset %q: status code %d: %s", folder, resp.StatusCode, data)
	}
	return nil
}

func allDevicesInSync(p []syncthingProcess) error {
	for _, device := range p {
		if err := device.allPeersInSync(); err != nil {
			return fmt.Errorf("%.7s: %v", device.id.String(), err)
		}
	}
	return nil
}
