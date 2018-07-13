package main

import (
	"sync"
	"io"
	"net"
	"time"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"encoding/json"
	"fmt"

	"gopkg.in/mgo.v2"
	"github.com/joeshaw/envdecode"
	"github.com/garyburd/go-oauth/oauth"
	"github.com/nsqio/go-nsq"
)

var conn net.Conn
func dial(netw, addr string) (net.Conn, error){
	if conn != nil {
		conn.Close()
		conn = nil
	}
	netc, err := net.DialTimeout(netw, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	conn = netc
	return netc, nil
}

var reader io.ReadCloser
func closeConn() {
	if conn != nil {
		conn.Close()
	}
	if reader != nil {
		reader.Close()
	}
}

var (
	authClient *oauth.Client
	creds *oauth.Credentials
)
func setupTwitterAuth() {
	var ts struct {
		ConsumerKey		string `env:"SP_TWITTER_KEY,required"`
		ConsumerSecret	string `env:"SP_TWITTER_SECRET,required"`
		AccessToken		string `env:"SP_TWITTER_ACCESSTOKEN,required"`
		AccessSecret	string `env:"SP_TWITTER_ACCESSSECRET,required"`
	}
	if err := envdecode.Decode(&ts); err != nil {
		log.Fatalln(err)
	}
	fmt.Println(ts)
	creds = &oauth.Credentials{
		Token: ts.AccessToken,
		Secret: ts.AccessSecret,
	}
	authClient = &oauth.Client{
		Credentials: oauth.Credentials {
			Token: ts.ConsumerKey,
			Secret: ts.ConsumerSecret,
		},
	}
}

var (
	authSetupOnce 	sync.Once
	httpClient 		*http.Client
)
func makeRequest(req *http.Request, params url.Values) (*http.Response, error) {
	authSetupOnce.Do(func() {
		setupTwitterAuth()
		httpClient = &http.Client{
			Transport: &http.Transport{
				Dial: dial,
			},
		}
	})
	formEnc := params.Encode()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Content-Length", strconv.Itoa(len(formEnc)))
	req.Header.Set("Authorization", authClient.AuthorizationHeader(creds, "POST", req.URL, params))
	return httpClient.Do(req)
}

var db *mgo.Session
var dbSettings struct { Address string `env:"MONGODB_ADDRESS,required"` }
func dialdb() error {
	var err error
	log.Println("dialing mongodb: " + dbSettings.Address)
	db, err = mgo.Dial("localhost:27018")
	return err
}
func closedb() {
	db.Close()
	log.Println("closed database connection")
}
type poll struct {
	Options []string
}
func loadoptions() ([]string, error) {
	var options []string
	iter := db.DB("ballots").C("pools").Find(nil).Iter()
	var p poll
	for iter.Next(&p) {
		options = append(options, p.Options...)
	}
	iter.Close()
	return options, iter.Err()
}

type tweet struct {
	Text string
}

func readFromTwitter(votes chan<- string) {
	options, err := loadoptions()
	fmt.Println(options)
	if err != nil {
		log.Println("failed to lad options:", err)
		return
	}
	u, err := url.Parse("https://stream.twitter.com/1.1/statuses/filter.json")
	if err != nil {
		log.Println("creating filter request failed:", err)
		return
	}
	query := make(url.Values)
	query.Set("track", strings.Join(options, ","))
	fmt.Println(query)
	req, err := http.NewRequest("POST", u.String(), strings.NewReader(query.Encode()))
	fmt.Println(req)
	if err != nil {
		log.Println("creating filter request failed:", err)
		return
	}
	resp, err := makeRequest(req, query)
	if err != nil {
		log.Println("making request failed:", err)
		return
	}
	reader := resp.Body
	decoder := json.NewDecoder(reader)
	fmt.Println(reader)
	fmt.Println(decoder)
	for {
		var t tweet
		if err := decoder.Decode(&t); err != nil {
			fmt.Println(t)
			fmt.Println(decoder.Decode(&t))
			fmt.Println(err)
			break
		}
		for _, option := range options {
			if strings.Contains(
				strings.ToLower(t.Text),
				strings.ToLower(option),
			) {
				log.Println("vote:", option)
				votes <- option
			}
		}
	}
}

func startTwitterStream(stopchan <-chan struct{}, votes chan<- string) <-chan struct{} {
	stoppedchan := make(chan struct{}, 1)
	go func() {
		defer func() {
			stoppedchan <- struct{}{}
		}()
		for {
			select {
			case <-stopchan:
				log.Println("stopping Twitter...")
				return
			default:
				log.Println("Quering Twitter...")
				readFromTwitter(votes)
				log.Println(" (waiting)")
				time.Sleep(10 * time.Second)
			}
		}
	}()
	return stoppedchan
}
var nsqSettings struct { Address string `env:"NSQ_ADDRESS,required"` }

func publishVotes(votes <-chan string) <-chan struct{} {
	stopchan := make(chan struct{}, 1)
	pub, err := nsq.NewProducer("localhost:4150", nsq.NewConfig())
	if err != nil {
		log.Println("there is an error in the vote channel")
	}
	go func() {
		for vote := range votes {
			pub.Publish("votes", []byte(vote))
		}
		log.Println("Publisher: Stopping")
		pub.Stop()
		log.Println("Publisher: Stopped")
		stopchan <- struct{}{}
	}()
	return stopchan
}