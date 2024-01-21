//go:generate swag init --parseDependency  --parseInternal -g .\api.go

package revcatgtpservice

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"emperror.dev/errors"
	"fmt"
	"github.com/Masterminds/sprig"
	"github.com/bluele/gcache"
	"github.com/gin-gonic/gin"
	"github.com/gosimple/slug"
	"github.com/je4/revcat/v2/tools/client"
	"github.com/je4/revcatgpt/v2/data/templates"
	"github.com/je4/revcatgpt/v2/pkg/revcatgtpservice/docs"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/je4/zsearch/v2/pkg/translate"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/pemistahl/lingua-go"
	"github.com/sashabaranov/go-openai"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"golang.org/x/net/http2"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"text/template"
)

const BASEPATH = "/api/v1"

//	@title			RevcatGPT API
//	@version		1.0
//	@description	Custom GPT Metadata for Revcat
//	@termsOfService	http://swagger.io/terms/

//	@contact.name	Jürgen Enge
//	@contact.url	https://info-age.ch
//	@contact.email	juergen@info-age.ch

//	@license.name	Apache 2.0
//	@license.url	http://www.apache.org/licenses/LICENSE-2.0.html

var languageNamer = map[string]display.Namer{
	"de": display.German.Tags(),
	"en": display.English.Tags(),
	"fr": display.French.Tags(),
	"it": display.Italian.Tags(),
}

func funcMap(bundle *i18n.Bundle) template.FuncMap {
	fm := sprig.FuncMap()

	fm["langName"] = func(langSrc, langTarget string) string {
		if namer, ok := languageNamer[langTarget]; ok {
			return namer.Name(language.MustParse(langSrc))
		}
		return langSrc
	}

	fm["localize"] = func(key, lang string) string {
		localizer := i18n.NewLocalizer(bundle, lang)

		result, err := localizer.LocalizeMessage(&i18n.Message{
			ID: key,
		})
		if err != nil {
			return key
			// return fmt.Sprintf("cannot localize '%s': %v", key, err)
		}
		return result // fmt.Sprintf("%s (%s)", result, lang)
	}
	fm["slug"] = func(s string, lang string) string {
		return strings.Replace(slug.MakeLang(s, lang), "-", "_", -1)
	}

	type size struct {
		Width  int64 `json:"width"`
		Height int64 `json:"height"`
	}
	fm["calcAspectSize"] = func(width, height, maxWidth, maxHeight int64) size {
		aspect := float64(width) / float64(height)
		maxAspect := float64(maxWidth) / float64(maxHeight)
		if aspect > maxAspect {
			return size{
				Width:  maxWidth,
				Height: int64(float64(maxWidth) / aspect),
			}
		} else {
			return size{
				Width:  int64(float64(maxHeight) * aspect),
				Height: maxHeight,
			}
		}
	}
	fm["multiLang"] = func(mf []*client.MultiLangFragment) *translate.MultiLangString {
		if len(mf) == 0 {
			return nil
		}
		m := &translate.MultiLangString{}
		for _, f := range mf {
			lang, err := language.Parse(f.Lang)
			if err != nil {
				lang = language.English
			}
			m.Set(f.Value, lang, f.Translated)
		}
		return m
	}
	return fm
}

func NewController(addr, extAddr string, cert *tls.Certificate, revcatClient client.RevCatGraphQLClient, bundle *i18n.Bundle, oaiClient *openai.Client, logger zLogger.ZLogger) (*controller, error) {
	u, err := url.Parse(extAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid external address '%s'", extAddr)
	}
	subpath := "/" + strings.Trim(u.Path, "/")

	// programmatically set swagger info
	docs.SwaggerInfo.Host = strings.TrimRight(fmt.Sprintf("%s:%s", u.Hostname(), u.Port()), " :")
	docs.SwaggerInfo.BasePath = "/" + strings.Trim(subpath+BASEPATH, "/")
	if cert == nil {
		docs.SwaggerInfo.Schemes = []string{"http"}
	} else {
		docs.SwaggerInfo.Schemes = []string{"https"}
	}

	router := gin.Default()
	var langs = []lingua.Language{}
	for _, lang := range bundle.LanguageTags() {
		l := lingua.GetLanguageFromIsoCode639_1(
			lingua.GetIsoCode639_1FromValue(lang.String()),
		)
		langs = append(langs, l)
	}
	tpl, err := template.New("embedding.gotmpl").Funcs(funcMap(bundle)).Parse(templates.EmbeddingTemplate)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse embedding template")
	}

	c := &controller{
		addr:         addr,
		router:       router,
		subpath:      subpath,
		cache:        gcache.New(100).LRU().Build(),
		logger:       logger,
		client:       revcatClient,
		bundle:       bundle,
		langDetector: lingua.NewLanguageDetectorBuilder().FromLanguages(langs...).Build(),
		oaiClient:    oaiClient,
		template:     tpl,
	}
	if err := c.Init(cert); err != nil {
		return nil, errors.Wrap(err, "cannot initialize rest controller")
	}
	return c, nil
}

type controller struct {
	server       http.Server
	router       *gin.Engine
	addr         string
	subpath      string
	cache        gcache.Cache
	logger       zLogger.ZLogger
	bundle       *i18n.Bundle
	client       client.RevCatGraphQLClient
	langDetector lingua.LanguageDetector
	oaiClient    *openai.Client
	template     *template.Template
}

func (ctrl *controller) Init(cert *tls.Certificate) error {
	v1 := ctrl.router.Group(BASEPATH)
	v1.GET("/:query", ctrl.chatSearch)

	ctrl.router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	//ctrl.router.StaticFS("/swagger/", http.FS(swaggerFiles.FS))

	var tlsConfig *tls.Config
	if cert != nil {
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{*cert},
		}
	}
	ctrl.server = http.Server{
		Addr:      ctrl.addr,
		Handler:   ctrl.router,
		TLSConfig: tlsConfig,
	}

	if err := http2.ConfigureServer(&ctrl.server, nil); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (ctrl *controller) Start(wg *sync.WaitGroup) {
	go func() {
		wg.Add(1)
		defer wg.Done() // let main know we are done cleaning up

		if ctrl.server.TLSConfig == nil {
			fmt.Printf("starting server at http://%s\n", ctrl.addr)
			if err := ctrl.server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				// unexpected error. port in use?
				fmt.Errorf("server on '%s' ended: %v", ctrl.addr, err)
			}
		} else {
			fmt.Printf("starting server at https://%s\n", ctrl.addr)
			if err := ctrl.server.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
				// unexpected error. port in use?
				fmt.Errorf("server on '%s' ended: %v", ctrl.addr, err)
			}
		}
		// always returns error. ErrServerClosed on graceful close
	}()
}

func (ctrl *controller) Stop() {
	ctrl.server.Shutdown(context.Background())
}

func (ctrl *controller) GracefulStop() {
	ctrl.server.Shutdown(context.Background())
}

// chatSearch godoc
// @Summary      gets GPT query context to query
// @ID			 get-context-by-query
// @Description  based on a GPT chat query, similar documents are searched and returned as context
// @Tags         GND
// @Produce      plain
// @Param		 query path string true "chat query"
// @Success      200  {string}  string
// @Failure      400  {object}  revcatgtpservice.HTTPResultMessage
// @Failure      404  {object}  revcatgtpservice.HTTPResultMessage
// @Failure      500  {object}  revcatgtpservice.HTTPResultMessage
// @Router       /{query} [get]
func (ctrl *controller) chatSearch(c *gin.Context) {
	var err error
	query := c.Param("query")
	if query == "" {
		c.JSON(http.StatusBadRequest, HTTPResultMessage{Message: "query is empty"})
		return
	}

	var lang language.Tag
	if lng, exists := ctrl.langDetector.DetectLanguageOf(query); exists {
		lang, err = language.Parse(lng.IsoCode639_3().String())
		if err != nil {
			c.JSON(http.StatusBadRequest, HTTPResultMessage{Message: fmt.Sprintf("cannot parse language %s", lng.String())})
			return
		}
	} else {
		lang = language.English
	}

	key := sha1.Sum([]byte(query))
	var embedding []float32
	if embeddingInterface, err := ctrl.cache.Get(key); err == nil {
		embedding, _ = embeddingInterface.([]float32)
	}
	if len(embedding) == 0 {
		// Create an EmbeddingRequest for the user query
		queryReq := openai.EmbeddingRequest{
			Input: []string{query},
			Model: openai.AdaEmbeddingV2,
		}
		embeddingResponse, err := ctrl.oaiClient.CreateEmbeddings(context.Background(), queryReq)
		if err != nil {
			c.JSON(http.StatusInternalServerError, HTTPResultMessage{Message: fmt.Sprintf("cannot create embedding for query %s - %v", query, err)})
			return
		}
		if len(embeddingResponse.Data) == 0 {
			c.JSON(http.StatusInternalServerError, HTTPResultMessage{Message: fmt.Sprintf("no embedding returned for query %s", query)})
			return
		}
		embedding = embeddingResponse.Data[0].Embedding
		ctrl.cache.Set(key, embedding)
	}
	var embedding64 = make([]float64, len(embedding))
	for i, v := range embedding {
		embedding64[i] = float64(v)
	}

	result, err := ctrl.client.VectorSearchShort(context.Background(), nil, embedding64, 30)
	if err != nil {
		c.JSON(http.StatusInternalServerError, HTTPResultMessage{Message: fmt.Sprintf("cannot search for query %s: %v", query, err)})
		return
	}
	var context string
	var tokens int
	for _, edge := range result.VectorSearch.Edges {
		data := struct {
			Lang   string
			Source *client.VectorSearchShort_VectorSearch_Edges
		}{
			Lang:   lang.String(),
			Source: edge,
		}
		buf := &bytes.Buffer{}
		if err := ctrl.template.Execute(buf, data); err != nil {
			c.JSON(http.StatusInternalServerError, HTTPResultMessage{Message: fmt.Sprintf("cannot execute template for query %s: %v", query, err)})
			return
		}
		tokens += NumTokensFromMessages([]openai.ChatCompletionMessage{
			openai.ChatCompletionMessage{
				Content: buf.String(),
			},
		}, "gpt-4-0314")
		context += fmt.Sprintf("%s\n\n---\n\n", buf.String())
		if tokens > 3000 {
			break
		}
	}
	ctrl.logger.Info().Msgf("tokens: %d", tokens)
	c.Data(http.StatusOK, "text/plain", []byte(context))
}
