package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"

	"github.com/myfreeweb/443d/demux"
	"github.com/myfreeweb/443d/keepalive"
	"github.com/ryanuber/go-glob"
	"golang.org/x/net/http2"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Tls struct {
		Listen string
		Cert   string
		Key    string
		Ssh    string
		Hsts   struct {
			Seconds    int
			Subdomains bool
		}
		Hpkp struct {
			Seconds    int
			Subdomains bool
			BackupKeys []string `yaml:"backup_keys"`
		}
	}
	Http struct {
		Listen string
	}
	Redirector struct {
		Listen string
	}
	Hosts       []HttpBackend
	DefaultHost string
}

var confpath = flag.String("config", "/usr/local/etc/443d.yaml", "path to the configuration file")
var config Config
var tlsKeyPair tls.Certificate
var hstsHeader string
var hpkpHeader string

var httpHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.Host == "" {
		r.Host = config.DefaultHost
	}
	for hostid := range config.Hosts {
		hostcnf := config.Hosts[hostid]
		for hostnid := range hostcnf.Hostnames {
			hostn := hostcnf.Hostnames[hostnid]
			if glob.Glob(hostn, r.Host) {
				hostcnf.Handler.ServeHTTP(w, r)
				return
			}
		}
	}
})

var redirHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
})

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	readConfig()
	processConfig()
	errc := make(chan error, 1)
	go func() {
		addr := config.Redirector.Listen
		if addr == "" {
			log.Printf("No listen address for the Redirector server \n")
			return
		}
		srv := &http.Server{Addr: addr, Handler: redirHandler}
		tcpl := listen(addr)
		kal := keepalive.KeepAliveListener{tcpl.(*net.TCPListener)}
		errc <- serve("Redirector server", srv, kal)
	}()
	go func() {
		addr := config.Http.Listen
		if addr == "" {
			log.Printf("No listen address for the HTTP server \n")
			return
		}
		srv := &http.Server{Addr: addr, Handler: httpHandler}
		tcpl := listen(addr)
		kal := keepalive.KeepAliveListener{tcpl.(*net.TCPListener)}
		errc <- serve("HTTP server", srv, kal)
	}()
	go func() {
		addr := config.Tls.Listen
		if addr == "" {
			log.Printf("No listen address for the TLS server \n")
			return
		}
		if config.Tls.Cert == "" && config.Tls.Key == "" {
			log.Printf("No keypair for the TLS server \n")
			return
		}
		secHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if config.Tls.Hsts.Seconds != 0 {
				w.Header().Add("Strict-Transport-Security", hstsHeader)
			}
			if config.Tls.Hpkp.Seconds != 0 {
				w.Header().Add("Public-Key-Pins", hpkpHeader)
			}
			httpHandler.ServeHTTP(w, r)
		})
		srv := &http.Server{Addr: addr, Handler: secHandler}
		http2.ConfigureServer(srv, &http2.Server{})
		srv.TLSConfig.Certificates = []tls.Certificate{tlsKeyPair}
		tcpl := listen(addr)
		sshh := demux.SshHandler(config.Tls.Ssh)
		dl := demux.DemultiplexingListener{tcpl.(*net.TCPListener), sshh}
		tlsl := tls.NewListener(dl, srv.TLSConfig)
		errc <- serve("TLS server", srv, tlsl)
	}()
	log.Fatalf("error: %v :-(\n", <-errc)
}

func listen(addr string) net.Listener {
	tcpl, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("%v :-(\n", err)
	}
	return tcpl
}

func serve(name string, srv *http.Server, listener net.Listener) error {
	log.Printf("Starting the "+name+" on tcp %v\n", srv.Addr)
	return srv.Serve(listener)
}

func readConfig() {
	flag.Parse()
	f, err := os.Open(*confpath)
	if err != nil {
		log.Fatalf("%v :-(\n", err)
	}
	defer f.Close()
	buf, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatalf("%v :-(\n", err)
	}
	if err := yaml.Unmarshal(buf, &config); err != nil {
		log.Fatalf("%v :-(\n", err)
	}
	for ib := range config.Hosts {
		config.Hosts[ib].Initialize()
	}
}

func processConfig() {
	if config.DefaultHost == "" {
		config.DefaultHost = "localhost"
	}
	if config.Tls.Cert != "" && config.Tls.Key != "" {
		var err error
		tlsKeyPair, err = tls.LoadX509KeyPair(config.Tls.Cert, config.Tls.Key)
		if err != nil {
			log.Fatalf("Error reading TLS key/cert: %v :-(", err)
		}
		tlsKeyPair.Leaf, err = x509.ParseCertificate(tlsKeyPair.Certificate[len(tlsKeyPair.Certificate)-1])
		if err != nil {
			log.Fatalf("Error parsing TLS cert: %v :-(", err)
		}
		if config.Tls.Hsts.Seconds != 0 {
			hstsHeader = fmt.Sprintf("max-age=%d", config.Tls.Hsts.Seconds)
			if config.Tls.Hsts.Subdomains {
				hstsHeader += "; includeSubdomains"
			}
		}
		if config.Tls.Hpkp.Seconds != 0 {
			if len(config.Tls.Hpkp.BackupKeys) < 1 {
				log.Printf("You should add a backup key to HPKP backup_keys!\n")
			}
			hash := sha256.Sum256(tlsKeyPair.Leaf.RawSubjectPublicKeyInfo)
			hpkpHeader = fmt.Sprintf("pin-sha256=\"%s\"", base64.StdEncoding.EncodeToString(hash[0:]))
			for k := range config.Tls.Hpkp.BackupKeys {
				hpkpHeader += fmt.Sprintf("; pin-sha256=\"%s\"", config.Tls.Hpkp.BackupKeys[k])
			}
			hpkpHeader += fmt.Sprintf("; max-age=%d", config.Tls.Hpkp.Seconds)
			if config.Tls.Hpkp.Subdomains {
				hpkpHeader += "; includeSubdomains"
			}
		}
	}
}
