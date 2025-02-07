package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/go-github/v50/github"
)

const (
	defaultHost        = "127.0.0.1"
	defaultPort        = "10000"
	defaultUpstreamURL = "https://ghcr.io"
)

type containerProxy struct {
	ghClient GitHubClient
}

// NewProxy returns an instance of container proxy, which implements the Docker
// Registry HTTP API V2.
func NewProxy(addr string, ghClient GitHubClient, rawUpstreamURL string) *http.Server {
	proxy := containerProxy{
		ghClient: ghClient,
	}

	// Create an upstream (reverse) proxy to handle the requests not supported by
	// the container proxy.
	upstreamURL, err := url.Parse(rawUpstreamURL)
	if err != nil {
		log.Fatal(err)
	}
	upstreamProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstreamURL)
		},
	}

	router := chi.NewRouter()
	// Set a timeout value on the request context (ctx), that will signal through
	// ctx.Done() that the request has timed out and further processing should be
	// stopped.
	router.Use(middleware.Timeout(30 * time.Second))

	router.Get("/v2/_catalog", proxy.Catalog)
	router.Get("/v2/{owner}/{name}/tags/list", proxy.TagsList)
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Not Found %s %s -> %s", r.Method, r.URL, upstreamURL)
		upstreamProxy.ServeHTTP(w, r)
	})

	return &http.Server{
		Addr:    addr,
		Handler: router,
	}
}

func GitHubUsers() []string {
	users := strings.Split(os.Getenv("GITHUB_USERS"), ",")
	if os.Getenv("GITHUB_USERS") != "" {
		defaultUser := []string{""}
		users = append(defaultUser, users...)
	}
	log.Printf("GitHub Users %s", strings.Join(users, ","))

	return users
}

// Catalog returns the list of repositories available in the Container Registry.
func (p *containerProxy) Catalog(w http.ResponseWriter, r *http.Request) {
	log.Printf("Catalog Request %s -> %s", r.Method, r.URL)
	users := GitHubUsers()
	w.Header().Set("Content-Type", "application/json")

	// Fetch the list of container packages the current user has access to.
	opts := &github.PackageListOptions{PackageType: &packageType}

	var successes int = 0
	var packages []*github.Package
	var errors apiErrors
	for _, user := range users {
		var newPackages int = 0
		tempPackages, _, err := p.ghClient.ListPackages(r.Context(), user, opts)
		if err != nil {
			log.Printf("WARN ListPackages for \"%s\" error: %s", user, err)
			error := apiError{Code: ERROR_UNKNOWN, Message: fmt.Sprintf("ListPackages: %s", err)}
			errors.Errors = append(errors.Errors, error)
		} else {
			successes++
			for _, tempPack := range tempPackages {
				if tempPack.Name == nil || tempPack.Owner.Login == nil {
					continue
				}
				var found bool = false
				for _, pack := range packages {
					if *tempPack.Name == *pack.Name && *tempPack.Owner.Login == *pack.Owner.Login {
						found = true
						break
					}
				}
				if !found {
					packages = append(packages, tempPack)
					newPackages++
				}
			}
			log.Printf("ListPackages for \"%s\" found %d _new_ packages", user, newPackages)
		}
	}

	if successes == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(&errors)
		return
	}

	// packages, _, err := p.ghClient.ListPackages(r.Context(), "", opts)
	// if err != nil {
	// 	w.WriteHeader(http.StatusBadRequest)
	// 	errors := makeError(ERROR_UNKNOWN, fmt.Sprintf("ListPackages: %s", err))
	// 	json.NewEncoder(w).Encode(&errors)
	// 	return
	// }

	catalog := struct {
		Repositories []string `json:"repositories"`
	}{
		Repositories: []string{},
	}
	for _, pack := range packages {
		if pack.Name == nil || pack.Owner.Login == nil {
			continue
		}

		catalog.Repositories = append(
			catalog.Repositories,
			fmt.Sprintf("%s/%s", *pack.Owner.Login, *pack.Name),
		)
	}
	json.NewEncoder(w).Encode(catalog)
}

// TagsList returns the list of tags for a given repository.
func (p *containerProxy) TagsList(w http.ResponseWriter, r *http.Request) {
	log.Printf("TagList Request %s -> %s", r.Method, r.URL)
	w.Header().Set("Content-Type", "application/json")

	owner := chi.URLParam(r, "owner")
	name := chi.URLParam(r, "name")

	versions, _, err := p.ghClient.PackageGetAllVersions(r.Context(), owner, packageType, name, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		errors := makeError(ERROR_UNKNOWN, fmt.Sprintf("PackageGetAllVersions: %s", err))
		json.NewEncoder(w).Encode(errors)
		return
	}

	list := struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{
		Name: fmt.Sprintf("%s/%s", owner, name),
		Tags: []string{},
	}
	for _, version := range versions {
		if version.Metadata == nil || version.Metadata.Container == nil {
			continue
		}

		list.Tags = append(
			list.Tags,
			version.Metadata.Container.Tags...,
		)
	}
	json.NewEncoder(w).Encode(list)
}

func main() {
	host := os.Getenv("HOST")
	if host == "" {
		host = defaultHost
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	addr := fmt.Sprintf("%s:%s", host, port)

	rawUpstreamURL := os.Getenv("UPSTREAM_URL")
	if rawUpstreamURL == "" {
		rawUpstreamURL = defaultUpstreamURL
	}

	// Create a GitHub client to call the REST API.
	ctx := context.Background()
	client := github.NewTokenClient(ctx, os.Getenv("GITHUB_TOKEN"))

	proxy := NewProxy(addr, client.Users, rawUpstreamURL)

	log.Printf("starting container registry proxy on %s", addr)
	log.Fatal(proxy.ListenAndServe())
}
