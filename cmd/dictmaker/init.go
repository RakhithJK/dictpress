package main

import (
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"plugin"
	"strings"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/dictmaker/internal/data"
	"github.com/knadh/koanf"
	"github.com/knadh/stuffbin"
)

// connectDB initializes a database connection.
func connectDB(host string, port int, user, pwd, dbName string) (*sqlx.DB, error) {
	db, err := sqlx.Connect("postgres",
		fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", host, port, user, pwd, dbName))
	if err != nil {
		return nil, err
	}

	return db, nil
}

// initFileSystem initializes the stuffbin FileSystem to provide
// access to bunded static assets to the app.
func initFileSystem() (stuffbin.FileSystem, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}

	fs, err := stuffbin.UnStuff(path)
	if err == nil {
		return fs, nil
	}

	// Running in local mode. Load the required static assets into
	// the in-memory stuffbin.FileSystem.
	logger.Printf("unable to initialize embedded filesystem: %v", err)
	logger.Printf("using local filesystem for static assets")

	files := []string{
		"config.toml.sample",
		"queries.sql",
		"schema.sql",
	}

	fs, err = stuffbin.NewLocalFS("/", files...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize local file for assets: %v", err)
	}

	return fs, nil
}

// loadSiteTheme loads a theme from a directory.
func loadSiteTheme(path string, loadPages bool) (*template.Template, error) {
	t := template.New("theme")

	// Helper functions.
	t = t.Funcs(template.FuncMap{"JoinStrings": strings.Join})
	t = t.Funcs(template.FuncMap{"ToUpper": strings.ToUpper})
	t = t.Funcs(template.FuncMap{"ToLower": strings.ToLower})
	t = t.Funcs(template.FuncMap{"Title": strings.Title})

	// Go percentage encodes unicode characters printed in <a href>,
	// but the encoded values are in lowercase hex (for some reason)
	// See: https://github.com/golang/go/issues/33596
	t = t.Funcs(template.FuncMap{"UnicodeURL": func(s string) template.URL {
		return template.URL(url.PathEscape(s))
	}})

	_, err := t.ParseGlob(path + "/*.html")
	if err != nil {
		return t, err
	}

	// Load arbitrary pages from (site_dir/pages/*.html).
	// For instance, "about" for site_dir/pages/about.html will be
	// rendered on site.com/pages/about where the template is defined
	// with the name {{ define "page-about" }}. All template name definitions
	// should be "page-*".
	if loadPages {
		if _, err := t.ParseGlob(path + "/pages/*.html"); err != nil {
			return t, err
		}
	}

	return t, nil
}

// initAdminTemplates loads admin UI HTML templates.
func initAdminTemplates(path string) *template.Template {
	t, err := template.New("admin").ParseGlob(path + "/*.html")
	if err != nil {
		log.Fatalf("error loading admin templates: %v", err)
	}
	return t
}

// loadTokenizerPlugin loads a tokenizer plugin that implements data.Tokenizer
// from the given path.
func loadTokenizerPlugin(path string) (data.Tokenizer, error) {
	plg, err := plugin.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error loading tokenizer plugin '%s': %v", path, err)
	}

	newFunc, err := plg.Lookup("New")
	if err != nil {
		return nil, fmt.Errorf("New() function not found in plugin '%s': %v", path, err)
	}

	f, ok := newFunc.(func() (data.Tokenizer, error))
	if !ok {
		return nil, fmt.Errorf("New() function is of invalid type in plugin '%s'", path)
	}

	// Initialize the plugin.
	p, err := f()
	if err != nil {
		return nil, fmt.Errorf("error initializing provider plugin '%s': %v", path, err)
	}

	return p, err
}

// initHandlers registers HTTP handlers.
func initHandlers(r *chi.Mux, app *App) {
	r.Use(middleware.StripSlashes)

	// Dictionary site HTML views.
	if app.constants.Site != "" {
		r.Get("/", wrap(app, handleIndexPage))
		r.Get("/dictionary/{fromLang}/{toLang}/{q}", wrap(app, handleSearchPage))
		r.Get("/dictionary/{fromLang}/{toLang}", wrap(app, handleGlossaryPage))
		r.Get("/glossary/{fromLang}/{toLang}/{initial}", wrap(app, handleGlossaryPage))
		r.Get("/glossary/{fromLang}/{toLang}", wrap(app, handleGlossaryPage))
		r.Get("/pages/{page}", wrap(app, handleStaticPage))

		// Static files.
		fs := http.StripPrefix("/static", http.FileServer(
			http.Dir(filepath.Join(app.constants.Site, "static"))))
		r.Get("/static/*", fs.ServeHTTP)
	} else {
		// API greeting if there's no site.
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			sendResponse("welcome to dictmaker", http.StatusOK, w)
		})
	}

	// Admin handlers.
	r.Get("/admin/static/*", http.StripPrefix("/admin/static", http.FileServer(http.Dir("admin/static"))).ServeHTTP)
	r.Get("/admin", wrap(app, adminPage("index")))
	r.Get("/admin/search", wrap(app, adminPage("search")))
	r.Get("/admin/entries/{guid}", wrap(app, adminPage("entry")))

	// APIs.
	r.Get("/api/config", wrap(app, handleGetConfig))
	r.Get("/api/stats", wrap(app, handleGetStats))
	r.Post("/api/entries", wrap(app, handleInsertEntry))
	r.Get("/api/entries/{guid}", wrap(app, handleGetEntry))
	r.Get("/api/entries/{guid}/parents", wrap(app, handleGetParentEntries))
	r.Delete("/api/entries/{guid}", wrap(app, handleDeleteEntry))
	r.Delete("/api/entries/{fromGuid}/relations/{toGuid}", wrap(app, handleDeleteRelation))
	r.Post("/api/entries/{fromGuid}/relations/{toGuid}", wrap(app, handleAddRelation))
	r.Put("/api/entries/{guid}/relations/weights", wrap(app, handleReorderRelations))
	r.Put("/api/entries/{guid}/relations/{relID}", wrap(app, handleUpdateRelation))
	r.Put("/api/entries/{guid}", wrap(app, handleUpdateEntry))
	r.Get("/api/dictionary/{fromLang}/{toLang}/{q}", wrap(app, handleSearch))
}

// initLangs loads language configuration into a given *App instance.
func initLangs(ko *koanf.Koanf) data.LangMap {
	out := make(data.LangMap)

	// Language configuration.
	for _, l := range ko.MapKeys("lang") {
		lang := data.Lang{Types: make(map[string]string)}
		if err := ko.UnmarshalWithConf("lang."+l, &lang, koanf.UnmarshalConf{Tag: "json"}); err != nil {
			log.Fatalf("error loading languages: %v", err)
		}

		// Load external plugin.
		logger.Printf("language: %s", l)

		if lang.TokenizerType == "plugin" {
			tk, err := loadTokenizerPlugin(lang.TokenizerName)
			if err != nil {
				log.Fatalf("error loading tokenizer plugin for %s: %v", l, err)
			}

			lang.Tokenizer = tk

			// Tokenizations for search queries are looked up by the tokenizer
			// ID() returned by the plugin and not the filename in the config.
			lang.TokenizerName = tk.ID()
			logger.Printf("loaded tokenizer %s", lang.TokenizerName)
		}

		out[l] = lang
	}

	return out
}

func generateNewFiles() error {
	if _, err := os.Stat("config.toml"); !os.IsNotExist(err) {
		return errors.New("config.toml exists. Remove it to generate a new one")
	}

	// Initialize the static file system into which all
	// required static assets (.sql, .js files etc.) are loaded.
	fs, err := initFileSystem()
	if err != nil {
		return err
	}

	// Generate config file.
	b, err := fs.Read("config.toml.sample")
	if err != nil {
		return fmt.Errorf("error reading sample config (is binary stuffed?): %v", err)
	}

	if err := ioutil.WriteFile("config.toml", b, 0644); err != nil {
		return err
	}

	return nil
}
