package obligator

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ip2location/ip2location-go/v9"
)

const IdentityTypeEmail = "email"

type Identity struct {
	IdType        string `json:"id_type"`
	Id            string `json:"id"`
	ProviderName  string `json:"provider_name"`
	Name          string `json:"name,omitempty"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

type Login struct {
	IdType       string `json:"id_type"`
	Id           string `json:"id"`
	ProviderName string `json:"provider_name"`
	Timestamp    string `json:"ts"`
}

type Server struct {
	api    *Api
	Config ServerConfig
	Mux    *ObligatorMux
	db     Database
	jose   *JOSE
	muxMap map[string]http.Handler
}

type ServerConfig struct {
	Port                   int
	AuthDomains            []string
	Prefix                 string
	DbPrefix               string
	Database               Database
	DatabaseDir            string
	ApiSocketDir           string
	BehindProxy            bool
	DisplayName            string
	GeoDbPath              string
	ForwardAuthPassthrough bool
	Domains                StringList
	Users                  StringList
	Public                 bool
	ProxyType              string
	LogoSvg                []byte
	DisableQrLogin         bool
	JwksJson               string
	OAuth2Providers        []*OAuth2Provider `json:"oauth2_providers"`
	Smtp                   *SmtpConfig       `json:"smtp"`
}

type StringList []string

func (d *StringList) String() string {
	s := ""
	for _, domain := range *d {
		s = s + " " + domain
	}
	return s
}

func (d *StringList) Set(value string) error {
	*d = append(*d, value)
	return nil
}

type SmtpConfig struct {
	Server     string `json:"server,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Port       int    `json:"port,omitempty"`
	Sender     string `json:"sender,omitempty"`
	SenderName string `json:"sender_name,omitempty"`
}

type OAuth2TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IdToken     string `json:"id_token,omitempty"`
}

type ObligatorMux struct {
	server      *Server
	behindProxy bool
	mux         *http.ServeMux
}

type UserinfoResponse struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

type Validation struct {
	Id     string `json:"id"`
	IdType string `json:"id_type"`
}

const RateLimitTime = 24 * time.Hour

// const RateLimitTime = 10 * time.Minute
const EmailValidationsPerTimeLimit = 12

func NewObligatorMux(behindProxy bool) *ObligatorMux {
	s := &ObligatorMux{
		behindProxy: behindProxy,
		mux:         http.NewServeMux(),
	}

	return s
}

func (s *ObligatorMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	// TODO: implement generic redirects so LastLogin stuff isn't hard
	// coded
	if r.Host == "lastlogin.io" {
		query := ""
		if r.URL.RawQuery != "" {
			query = "?" + r.URL.RawQuery
		}
		redirUri := fmt.Sprintf("https://lastlogin.net%s%s", r.URL.Path, query)
		http.Redirect(w, r, redirUri, 308)
		return
	}

	// TODO: mutex?
	mux, exists := s.server.muxMap[r.Host]
	if exists {
		validation, err := s.server.Validate(r)
		if err != nil {
			w.WriteHeader(401)
			io.WriteString(w, err.Error())
			return
		}

		newReq := r.Clone(context.Background())

		if validation != nil {
			newReq.Header.Set("Remote-Id-Type", validation.IdType)
			newReq.Header.Set("Remote-Id", validation.Id)
		} else {
			newReq.Header.Set("Remote-Id-Type", "")
			newReq.Header.Set("Remote-Id", "")
		}

		mux.ServeHTTP(w, newReq)
		return
	}

	// TODO: see if we can re-enable script-src none. Removed it for FedCM support
	//w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; script-src 'none'")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")

	timestamp := time.Now().Format(time.RFC3339)

	remoteIp, err := getRemoteIp(r, s.behindProxy)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}

	cookieDomain, err := buildCookieDomain(r.Host)
	if err != nil {
		w.WriteHeader(500)
		io.WriteString(w, err.Error())
		return
	}

	crossSiteDetectorCookie := &http.Cookie{
		Domain:   cookieDomain,
		Name:     "obligator_not_cross_site",
		Value:    "true",
		Secure:   true,
		HttpOnly: true,
		MaxAge:   86400 * 365,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, crossSiteDetectorCookie)

	fmt.Println(fmt.Sprintf("%s\t%s\t%s\t%s\t%s", timestamp, remoteIp, r.Method, r.Host, r.URL.Path))
	s.mux.ServeHTTP(w, r)
}

func (s *ObligatorMux) Handle(p string, h http.Handler) {
	s.mux.Handle(p, h)
}

func (s *ObligatorMux) HandleFunc(p string, f func(w http.ResponseWriter, r *http.Request)) {
	s.mux.HandleFunc(p, f)
}

//go:embed templates assets
var fs embed.FS

func NewServer(conf ServerConfig) *Server {

	if conf.Port == 0 {
		conf.Port = 1616
	}

	if conf.Prefix == "" {
		conf.Prefix = "obligator_"
	}

	if conf.DisplayName == "" {
		conf.DisplayName = "obligator"
	}

	if conf.ProxyType == "" {
		conf.ProxyType = "builtin"
	}

	var err error

	var db Database
	if conf.Database != nil {
		db = conf.Database
	} else {
		dbPath := filepath.Join(conf.DatabaseDir, conf.DbPrefix+"db.sqlite")
		db, err = NewSqliteDatabase(dbPath, conf.DbPrefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	}

	prefix, err := db.GetPrefix()
	checkErr(err)

	cluster := NewCluster()
	writable := cluster.IAmThePrimary()

	if writable {

		if conf.Smtp != nil {
			err := db.SetSmtpConfig(conf.Smtp)
			checkErr(err)
		}

		if conf.Prefix != "obligator_" || prefix == "" {
			db.SetPrefix(conf.Prefix)
		}

		if conf.DisplayName != "obligator" {
			db.SetDisplayName(conf.DisplayName)
		}

		for _, domain := range conf.Domains {
			db.AddDomain(domain, "root")
		}

		for _, userId := range conf.Users {
			err := db.SetUser(&User{
				IdType: "email",
				Id:     userId,
			})
			checkErr(err)
		}

		// TODO: re-enable
		//conf.AuthDomains = append(conf.AuthDomains, rootUrl.Host)

		if conf.ForwardAuthPassthrough {
			db.SetForwardAuthPassthrough(true)
		}

		if conf.Public {
			db.SetPublic(true)
		}

		if conf.OAuth2Providers != nil {
			for _, p := range conf.OAuth2Providers {
				err := db.SetOAuth2Provider(p)
				checkErr(err)
			}
		}

	}

	domains, err := db.GetDomains()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}

	if len(domains) == 0 {
		fmt.Fprintln(os.Stderr, "WARNING: No domains set")
	}

	prefix, err = db.GetPrefix()
	checkErr(err)

	proxy := NewProxy(&conf, prefix)

	for _, d := range domains {
		// TODO: was running this in goroutines, but not all the
		// domains were making it into Caddy. Need to make it work
		// in parallel
		err = proxy.AddDomain(d.Domain)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
		}
	}

	oauth2MetaMan := NewOAuth2MetadataManager(db)

	err = oauth2MetaMan.Update()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	api, err := NewApi(db, conf.ApiSocketDir, oauth2MetaMan)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	jose, err := NewJOSE(db, cluster)
	checkErr(err)

	tmpl, err := template.ParseFS(fs, "templates/*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	mux := NewObligatorMux(conf.BehindProxy)

	var geoDb *ip2location.DB
	if conf.GeoDbPath != "" {
		geoDb, err = ip2location.OpenDB(conf.GeoDbPath)
		if err != nil {
			fmt.Println(err.Error())
			return nil
		}
	}

	handler := NewHandler(db, conf, tmpl, jose)
	mux.Handle("/", handler)

	oidcHandler := NewOIDCHandler(db, conf, tmpl, jose)
	mux.Handle("/.well-known/openid-configuration", oidcHandler)
	mux.Handle("/jwks", oidcHandler)
	mux.Handle("/register", oidcHandler)
	mux.Handle("/userinfo", oidcHandler)
	mux.Handle("/auth", oidcHandler)
	mux.Handle("/approve", oidcHandler)
	mux.Handle("/token", oidcHandler)
	mux.Handle("/end-session", oidcHandler)

	addIdentityOauth2Handler := NewAddIdentityOauth2Handler(db, oauth2MetaMan, jose)
	mux.Handle("/login-oauth2", addIdentityOauth2Handler)
	mux.Handle("/callback", addIdentityOauth2Handler)

	addIdentityEmailHandler := NewAddIdentityEmailHandler(db, cluster, tmpl, conf.BehindProxy, geoDb, jose)
	mux.Handle("/login-email", addIdentityEmailHandler)
	mux.Handle("/email-sent", addIdentityEmailHandler)
	mux.Handle("/magic", addIdentityEmailHandler)
	mux.Handle("/confirm-magic", addIdentityEmailHandler)
	mux.Handle("/complete-email-login", addIdentityEmailHandler)

	addIdentityGamlHandler := NewAddIdentityGamlHandler(db, cluster, tmpl, jose)
	mux.Handle("/login-gaml", addIdentityGamlHandler)
	mux.Handle("/gaml-code", addIdentityGamlHandler)
	mux.Handle("/complete-gaml-login", addIdentityGamlHandler)

	qrHandler := NewQrHandler(db, cluster, tmpl, jose)
	mux.Handle("/login-qr", qrHandler)
	mux.Handle("/qr", qrHandler)
	mux.Handle("/send", qrHandler)
	mux.Handle("/receive", qrHandler)

	indieAuthPrefix := "/indieauth"
	indieAuthHandler := NewIndieAuthHandler(db, tmpl, indieAuthPrefix, jose)
	mux.Handle("/users/", indieAuthHandler)
	mux.Handle("/.well-known/oauth-authorization-server", indieAuthHandler)
	mux.Handle(indieAuthPrefix+"/", http.StripPrefix(indieAuthPrefix, indieAuthHandler))

	domainHandler := NewDomainHandler(db, tmpl, cluster, proxy, jose)
	mux.Handle("/domains", domainHandler)
	mux.Handle("/add-domain", domainHandler)

	fedCmLoginEndpoint := "/login-fedcm-auto"
	fedCmHandler := NewFedCmHandler(db, fedCmLoginEndpoint, jose)
	mux.Handle("/.well-known/web-identity", fedCmHandler)
	mux.Handle("/fedcm/", http.StripPrefix("/fedcm", fedCmHandler))

	addIdentityFedCmHandler := NewAddIdentityFedCmHandler(db, tmpl, jose)
	mux.Handle("/login-fedcm", addIdentityFedCmHandler)
	mux.Handle("/complete-login-fedcm", addIdentityFedCmHandler)

	s := &Server{
		Config: conf,
		Mux:    mux,
		api:    api,
		db:     db,
		jose:   jose,
		muxMap: make(map[string]http.Handler),
	}

	// TODO: very hacky
	mux.server = s

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Mux.ServeHTTP(w, r)
}

func (s *Server) Start() error {

	server := http.Server{
		Addr:    fmt.Sprintf(":%d", s.Config.Port),
		Handler: s.Mux,
	}

	fmt.Println("Running")

	err := server.ListenAndServe()
	if err != nil {
		fmt.Fprintf(os.Stderr, err.Error())
		return err
	}

	return nil
}

// TODO: re-enable
//func (s *Server) AuthUri(authReq *OAuth2AuthRequest) string {
//	return AuthUri(s.Config.RootUri+"/auth", authReq)
//}

func AuthUri(serverUri string, authReq *OAuth2AuthRequest) string {
	uri := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=%s&state=%s&scope=%s",
		serverUri, authReq.ClientId, authReq.RedirectUri,
		authReq.ResponseType, authReq.State, authReq.Scope)
	return uri
}

func (s *Server) AuthDomains() []string {
	return s.Config.AuthDomains
}

// TODO: use pointer
func (s *Server) SetOAuth2Provider(prov OAuth2Provider) error {
	return s.api.SetOAuth2Provider(&prov)
}

func (s *Server) AddUser(user User) error {
	return s.api.AddUser(user)
}

func (s *Server) GetUsers() ([]*User, error) {
	return s.api.GetUsers()
}

func (s *Server) Validate(r *http.Request) (*Validation, error) {
	return validate(s.db, r, s.jose)
}

func (s *Server) ProxyMux(domain string, mux http.Handler) error {
	s.muxMap[domain] = mux
	return nil
}

func checkErrPassthrough(err error, passthrough bool) (*Validation, error) {
	if passthrough {
		return nil, nil
	} else {
		return nil, err
	}
}

func validate(db Database, r *http.Request, jose *JOSE) (*Validation, error) {

	passthrough, err := db.GetForwardAuthPassthrough()
	if err != nil {
		return nil, err
	}

	loginKeyCookie, err := getLoginCookie(db, r)
	if err != nil {
		return checkErrPassthrough(err, passthrough)
	}

	parsed, err := jose.Parse(loginKeyCookie.Value)
	if err != nil {
		return checkErrPassthrough(err, passthrough)
	}

	tokIdentsInterface, exists := parsed.Get("identities")
	if !exists {
		return checkErrPassthrough(errors.New("No identities"), passthrough)
	}

	tokIdents, ok := tokIdentsInterface.([]*Identity)
	if !ok {
		return checkErrPassthrough(errors.New("No identities"), passthrough)
	}

	// TODO: maybe return whole list of identities?
	ident := tokIdents[0]

	v := &Validation{
		IdType: ident.IdType,
		Id:     ident.Id,
	}

	return v, nil
}

func checkErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
