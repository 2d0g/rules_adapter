// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"qim/common/utils"
	"reflect"
	"strings"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
	redis "gopkg.in/redis.v4"
	yaml "gopkg.in/yaml.v2"

	"github.com/prometheus/common/version"
	"github.com/tinytub/rules_adapter/pkg/rulefmt"
)

func main() {
	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling for the Prometheus monitoring system.")
	app.Version(version.Print("promtool"))
	app.HelpFlag.Short('h')

	/*
		checkCmd := app.Command("check", "Check the resources for validity.")

		checkRulesCmd := checkCmd.Command("rules", "Check if the rule files are valid or not.")
		ruleFiles := checkRulesCmd.Arg(
			"rule-files",
			"The rule files to check.",
		).Required().ExistingFiles()

		updateCmd := app.Command("update", "Update the resources to newer formats.")
		updateRulesCmd := updateCmd.Command("rules", "Update rules from the 1.x to 2.x format.")
		ruleFilesUp := updateRulesCmd.Arg("rule-files", "The rule files to update.").Required().ExistingFiles()
	*/

	updateCmd := app.Command("update", "Update the resources to newer formats.")
	ruleFilePath := updateCmd.Arg("path", "rules file path").Required().ExistingDir()

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	/*
		case checkRulesCmd.FullCommand():
			os.Exit(CheckRules(*ruleFiles...))

		case updateRulesCmd.FullCommand():
			os.Exit(UpdateRules(*ruleFilesUp...))
	*/

	case updateCmd.FullCommand():
		os.Exit(RefreshRules(*ruleFilePath))

	}

}

type judgeRecored struct {
	Name string `json:"alarm_name"`
	Expr string `json:"expre"`
}

func RefreshRules(path string) int {

	interval := time.Duration(60 * time.Second)

	for {
		select {
		case <-time.Tick(interval):
			updateRules()
		}
	}

}

func updateRules() int {

	filename := "test"

	data, err := getRedisData()
	if err != nil {
		fmt.Println("get data from redis error: ", err)
		return 1
	}

	remoteRulesMap, err := getRemoteRules(data)
	if err != nil {
		fmt.Println("get Remote rules error: ", err)
		return 1
	}

	//check rules
	rulenum, ruleGroups, errsLocal := checkLocalRules(filename + ".yml")

	//TODO: 文件不存在的时候该如何处理？
	if errsLocal != nil {
		fmt.Println("local rules err: ", errsLocal)
		//return 1
	}

	localrules := make([]rulefmt.Rule, 0, rulenum)
	for _, rg := range ruleGroups {
		localrules = append(localrules, rg.Rules...)
	}

	y, remoteRules, err := convertToYaml(remoteRulesMap, filename)
	errsRemote := checkRulesValid(remoteRules)

	if errsRemote != nil {
		fmt.Println("remote rules err: ", errsRemote)
	}

	ioutil.WriteFile(filename+".yml", y, 0666)

	newrules, newupdate := checkUpdate(localrules, remoteRules)

	isUpdate := false
	if len(newrules) > 0 || len(newupdate) > 0 {
		isUpdate = true
	}

	if isUpdate {
		//TODO: 暂时比较粗暴，rule全刷，如果碰到已存在配置文件的情况可能会把已有内容刷丢。
		//尽量配合newrules和newupdate列表进行变更。
		updateRulesFile(y, filename)
		reloadPromeConfig()
	}

	return 0

}

func getRedisData() ([]string, error) {
	client := redis.NewClient(&redis.Options{
		Network:  "tcp",
		Addr:     "10.139.103.34:6001",
		Password: "test",
		DB:       2,
	})
	defer client.Close()

	data, err := client.LRange("CUSTOM_EXPRESS_STRATEGY", 0, -1).Result()
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	fmt.Println("get redis data: ", data)
	return data, nil
}

func getRemoteRules(data []string) (map[string]string, error) {
	remoteRulesMap := make(map[string]string, len(data))
	for _, d := range data {
		var record judgeRecored
		err := json.Unmarshal([]byte(d), &record)
		if err != nil {
			return nil, err
		}
		remoteRulesMap[record.Name] = strings.TrimSpace(record.Expr)
	}

	return remoteRulesMap, nil
}

func convertToYaml(remoteRules map[string]string, filename string) ([]byte, []rulefmt.Rule, error) {
	//try write file
	yamlRG := &rulefmt.RuleGroups{
		Groups: []rulefmt.RuleGroup{{
			Name: filename,
		}},
	}

	yamlRules := make([]rulefmt.Rule, 0, len(remoteRules))

	for name, expr := range remoteRules {
		yamlRules = append(yamlRules, rulefmt.Rule{
			Record: name,
			Expr:   expr,
			Labels: map[string]string{"testlable1": "testvalue1", "testlabel2": "testvalue2"},
		})
	}

	yamlRG.Groups[0].Rules = yamlRules
	y, err := yaml.Marshal(yamlRG)

	if err != nil {
		fmt.Println(err)
		return nil, nil, err
	}
	return y, yamlRules, err
}

func checkRulesValid(data []rulefmt.Rule) []error {
	var errors []error
	for _, d := range data {
		errs := d.Validate()
		if errs != nil {
			errors = append(errors, errs...)
		}
	}
	return errors
}

func checkLocalRules(filename string) (int, []rulefmt.RuleGroup, []error) {
	fmt.Println("Checking", filename)

	rgs, errs := rulefmt.ParseFile(filename)
	if errs != nil {
		return 0, nil, errs
	}

	numRules := 0
	for _, rg := range rgs.Groups {
		numRules += len(rg.Rules)
	}

	return numRules, rgs.Groups, nil
}

func checkUpdate(localrule, remoterule []rulefmt.Rule) ([]string, []string) {
	var newrules []string
	var newupdates []string
	var nrule bool
	for _, rrule := range remoterule {
		nrule = true
		for _, lrule := range localrule {
			if lrule.Record == rrule.Record {
				nrule = false
				if lrule.Expr != rrule.Expr || !reflect.DeepEqual(lrule.Labels, rrule.Labels) {
					newupdates = append(newupdates, lrule.Record)
					break
				}
				break
			}
		}
		if nrule {
			newrules = append(newrules, rrule.Record)
		}
	}
	return newrules, newupdates
}

func updateRulesFile(data []byte, filename string) {
	ioutil.WriteFile(filename+".yml", data, 0666)
}

func reloadPromeConfig() {
	client, err := utils.NewClientForTimeOut()
	if err != nil {
		fmt.Println(err)
		return
	}
	url := "http://127.0.0.1:9090/-/reload"
	request, err := http.NewRequest("GET", url, strings.NewReader(""))
	if err != nil {
		fmt.Println(err)
		return
	}

	_, err = client.Do(request)

	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("prometheus reload Done")

}

func NewClientForTimeOut() (*http.Client, error) {

	timeout := time.Duration(3 * time.Second)
	var rt http.RoundTripper = NewDeadlineRoundTripper(timeout)

	// Return a new client with the configured round tripper.
	return NewClient(rt), nil
}

func NewDeadlineRoundTripper(timeout time.Duration) http.RoundTripper {
	return &http.Transport{
		DisableKeepAlives: true,
		Dial: func(netw, addr string) (c net.Conn, err error) {
			start := time.Now()

			c, err = net.DialTimeout(netw, addr, timeout)
			if err != nil {
				return nil, err
			}

			//TODO 超时打点
			if err = c.SetDeadline(start.Add(timeout)); err != nil {
				c.Close()
				return nil, err
			}

			return c, nil
		},
	}
}

func NewClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt}
}
