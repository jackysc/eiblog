// Package main provides ...
package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/snowflake"
	"github.com/eiblog/blackfriday"
	"github.com/eiblog/utils/logd"
	"github.com/jackysc/eiblog/setting"
	r "gopkg.in/gorethink/gorethink.v4"
)

// 数据库及表名
const (
	DB                 = "eiblog"
	COLLECTION_ACCOUNT = "account"
	COLLECTION_ARTICLE = "article"
	COUNTER_SERIE      = "serie"
	COUNTER_ARTICLE    = "article"
	SERIES_MD          = "series_md"
	ARCHIVE_MD         = "archive_md"
	ADD                = "add"
	DELETE             = "delete"
)

// blackfriday 配置
const (
	commonHtmlFlags = 0 |
		blackfriday.HTML_TOC |
		blackfriday.HTML_USE_XHTML |
		blackfriday.HTML_USE_SMARTYPANTS |
		blackfriday.HTML_SMARTYPANTS_FRACTIONS |
		blackfriday.HTML_SMARTYPANTS_DASHES |
		blackfriday.HTML_SMARTYPANTS_LATEX_DASHES |
		blackfriday.HTML_NOFOLLOW_LINKS

	commonExtensions = 0 |
		blackfriday.EXTENSION_NO_INTRA_EMPHASIS |
		blackfriday.EXTENSION_TABLES |
		blackfriday.EXTENSION_FENCED_CODE |
		blackfriday.EXTENSION_AUTOLINK |
		blackfriday.EXTENSION_STRIKETHROUGH |
		blackfriday.EXTENSION_SPACE_HEADERS |
		blackfriday.EXTENSION_HEADER_IDS |
		blackfriday.EXTENSION_BACKSLASH_LINE_BREAK |
		blackfriday.EXTENSION_DEFINITION_LISTS
)

// Global Account
var (
	Ei      *Account
	lock    sync.Mutex
	session *r.Session
	node    *snowflake.Node
)

func init() {
	var err error
	node, err = snowflake.NewNode(setting.Conf.SeqNode)
	session, err = r.Connect(r.ConnectOpts{
		Address:    fmt.Sprintf("%s:%s", setting.Conf.Database.Host, setting.Conf.Database.Port),
		Database:   setting.Conf.Database.DbName,
		Username:   setting.Conf.Database.Username,
		Password:   setting.Conf.Database.Password,
		InitialCap: setting.Conf.Database.InitialCap,
		MaxOpen:    setting.Conf.Database.MaxOpen,
	})
	if err != nil {
		panic(err)
	}
	r.TableCreate(COLLECTION_ACCOUNT, r.TableCreateOpts{
		PrimaryKey: "username",
	}).Run(session)
	r.TableCreate(COLLECTION_ARTICLE, r.TableCreateOpts{
		PrimaryKey: "id",
	}).Run(session)
	r.Table(COLLECTION_ARTICLE).IndexCreate("slug").Run(session)
	r.TableCreate(COUNTER_SERIE).Run(session)
	r.TableCreate(COUNTER_ARTICLE).Run(session)
	r.TableCreate(SERIES_MD).Run(session)
	r.TableCreate(ARCHIVE_MD).Run(session)
	r.TableCreate(ADD).Run(session)
	r.TableCreate(DELETE).Run(session)
	// 读取帐号信息
	loadAccount()
	// 获取文章数据
	loadArticles()
	// 生成markdown文档
	go generateMarkdown()
	// 启动定时器
	go timer()
	// 获取评论数量
	go PostsCount()
}

// 读取或初始化帐号信息
func loadAccount() {
	Ei = &Account{}
	cur, err := r.Table(COLLECTION_ACCOUNT).Filter(map[string]interface{}{"username": setting.Conf.Account.Username}).Run(session)
	if err == r.ErrEmptyResult {
		logd.Printf("Initializing account: %s\n", setting.Conf.Account.Username)
		Ei = &Account{
			Username:   setting.Conf.Account.Username,
			Password:   EncryptPasswd(setting.Conf.Account.Username, setting.Conf.Account.Password),
			Email:      setting.Conf.Account.Email,
			PhoneN:     setting.Conf.Account.PhoneNumber,
			Address:    setting.Conf.Account.Address,
			CreateTime: time.Now(),
		}
		Ei.BlogName = setting.Conf.Blogger.BlogName
		Ei.SubTitle = setting.Conf.Blogger.SubTitle
		Ei.BeiAn = setting.Conf.Blogger.BeiAn
		Ei.BTitle = setting.Conf.Blogger.BTitle
		Ei.Copyright = setting.Conf.Blogger.Copyright
		r.Table(COLLECTION_ACCOUNT).Insert(Ei)
		generateTopic()
	} else if err != nil {
		logd.Fatal(err)
	}
	err = cur.One(&Ei)
	if err != nil {
		logd.Fatal(err)
	}
	Ei.CH = make(chan string, 2)
	Ei.MapArticles = make(map[string]*Article)
	Ei.Tags = make(map[string]SortArticles)
}

func loadArticles() {
	cur, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"isdraft": false, "deletetime": time.Time{}}).Run(session)
	cur.All(&Ei.Articles)
	if err != nil {
		logd.Fatal(err)
	}
	sort.Sort(Ei.Articles)
	for i, v := range Ei.Articles {
		// 渲染文章
		GenerateExcerptAndRender(v)
		Ei.MapArticles[v.Slug] = v
		// 分析文章
		if v.ID < setting.Conf.General.StartID {
			continue
		}
		if i > 0 {
			v.Prev = Ei.Articles[i-1]
		}
		if Ei.Articles[i+1].ID >= setting.Conf.General.StartID {
			v.Next = Ei.Articles[i+1]
		}
		upArticle(v, false)
	}
	Ei.CH <- SERIES_MD
	Ei.CH <- ARCHIVE_MD
}

// generate series,archive markdown
func generateMarkdown() {
	for {
		switch typ := <-Ei.CH; typ {
		case SERIES_MD:
			sort.Sort(Ei.Series)
			var buffer bytes.Buffer
			buffer.WriteString(Ei.SeriesSay)
			buffer.WriteString("\n\n")
			for _, serie := range Ei.Series {
				buffer.WriteString(fmt.Sprintf("### %s{#toc-%d}", serie.Name, serie.ID))
				buffer.WriteString("\n")
				buffer.WriteString(serie.Desc)
				buffer.WriteString("\n\n")
				for _, artc := range serie.Articles {
					//eg. * [标题一](/post/hello-world.html) <span class="date">(Man 02, 2006)</span>
					buffer.WriteString("* [" + artc.Title + "](/post/" + artc.Slug +
						".html) <span class=\"date\">(" + artc.CreateTime.Format("Jan 02, 2006") + ")</span>\n")
				}
				buffer.WriteByte('\n')
			}
			Ei.PageSeries = string(renderPage(buffer.Bytes()))
		case ARCHIVE_MD:
			sort.Sort(Ei.Archives)
			var buffer bytes.Buffer
			buffer.WriteString(Ei.ArchivesSay + "\n")

			var (
				currentYear string
				gt12Month   = len(Ei.Archives) > 12
			)
			for _, archive := range Ei.Archives {
				if gt12Month {
					year := archive.Time.Format("2006 年")
					if currentYear != year {
						currentYear = year
						buffer.WriteString(fmt.Sprintf("\n### %s\n\n", archive.Time.Format("2006 年")))
					}
				} else {
					buffer.WriteString(fmt.Sprintf("\n### %s\n\n", archive.Time.Format("2006年1月")))
				}
				for i, artc := range archive.Articles {
					if i == 0 && gt12Month {
						buffer.WriteString("* *[" + artc.Title + "](/post/" + artc.Slug +
							".html) <span class=\"date\">(" + artc.CreateTime.Format("Jan 02, 2006") + ")</span>*\n")
					} else {
						buffer.WriteString("* [" + artc.Title + "](/post/" + artc.Slug +
							".html) <span class=\"date\">(" + artc.CreateTime.Format("Jan 02, 2006") + ")</span>\n")
					}
				}
			}
			Ei.PageArchives = string(renderPage(buffer.Bytes()))
		}
	}
}

// init account: generate blogroll and about page
func generateTopic() {
	about := &Article{
		ID:         node.Generate().Int64(),
		Author:     setting.Conf.Account.Username,
		Title:      "关于",
		Slug:       "about",
		CreateTime: time.Time{},
		UpdateTime: time.Time{},
	}
	// 推送到 disqus
	go func() { ThreadCreate(about) }()
	blogroll := &Article{
		ID:         node.Generate().Int64(),
		Author:     setting.Conf.Account.Username,
		Title:      "友情链接",
		Slug:       "blogroll",
		CreateTime: time.Time{},
		UpdateTime: time.Time{},
	}
	_, err := r.Table(COLLECTION_ARTICLE).Insert(blogroll).Run(session)
	if err != nil {
		logd.Fatal(err)
	}
	_, err = r.Table(COLLECTION_ARTICLE).Insert(about).Run(session)
	if err != nil {
		logd.Fatal(err)
	}
}

// render page
func renderPage(md []byte) []byte {
	renderer := blackfriday.HtmlRenderer(commonHtmlFlags, "", "")
	return blackfriday.Markdown(md, renderer, commonExtensions)
}

// 文章分页
func PageList(p, n int) (prev int, next int, artcs []*Article) {
	var l int
	for l = len(Ei.Articles); l > 0; l-- {
		if Ei.Articles[l-1].ID >= setting.Conf.General.StartID {
			break
		}
	}
	if l == 0 {
		return 0, 0, nil
	}
	m := l / n
	if d := l % n; d > 0 {
		m++
	}
	if p > m {
		p = m
	}
	if p > 1 {
		prev = p - 1
	}
	if p < m {
		next = p + 1
	}
	s := (p - 1) * n
	e := p * n
	if e > l {
		e = l
	}
	artcs = Ei.Articles[s:e]
	return
}

// 渲染markdown操作和截取摘要操作
var reg = regexp.MustCompile(setting.Conf.General.Identifier)

// header
var regH = regexp.MustCompile("</nav></div>")

func GenerateExcerptAndRender(artc *Article) {
	if strings.HasPrefix(artc.Content, setting.Conf.General.DescPrefix) {
		index := strings.Index(artc.Content, "\r\n")
		artc.Desc = IgnoreHtmlTag(artc.Content[len(setting.Conf.General.DescPrefix):index])
		artc.Content = artc.Content[index:]
	}

	// 查找目录
	content := renderPage([]byte(artc.Content))
	index := regH.FindIndex(content)
	if index != nil {
		artc.Header = string(content[0:index[1]])
		artc.Content = string(content[index[1]:])
	} else {
		artc.Content = string(content)
	}
	index = reg.FindStringIndex(artc.Content)
	if index != nil {
		artc.Excerpt = IgnoreHtmlTag(artc.Content[0:index[0]])
	} else {
		uc := []rune(artc.Content)
		length := setting.Conf.General.Length
		if len(uc) < length {
			length = len(uc)
		}
		artc.Excerpt = IgnoreHtmlTag(string(uc[0:length]))
	}
}

// 读取草稿箱
func LoadDraft() (artcs SortArticles, err error) {
	cur, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"isdraft": true}).Run(session)
	cur.All(&artcs)
	if err != nil {
		logd.Fatal(err)
	}
	sort.Sort(artcs)
	return
}

// 读取回收箱
func LoadTrash() (artcs SortArticles, err error) {
	cur, err := r.Table(COLLECTION_ARTICLE).Ne(r.Row.Field("deletetime"), time.Time{}).Run(session)
	cur.All(&artcs)
	if err != nil {
		logd.Fatal(err)
	}
	sort.Sort(artcs)
	return
}

// 添加文章到tag、serie、archive
func upArticle(artc *Article, needSort bool) {
	// tag
	for _, tag := range artc.Tags {
		Ei.Tags[tag] = append(Ei.Tags[tag], artc)
		if needSort {
			sort.Sort(Ei.Tags[tag])
		}
	}
	// serie
	for i, serie := range Ei.Series {
		if serie.ID == artc.SerieID {
			Ei.Series[i].Articles = append(Ei.Series[i].Articles, artc)
			if needSort {
				sort.Sort(Ei.Series[i].Articles)
				Ei.CH <- SERIES_MD
			}
			break
		}
	}
	// archive
	y, m, _ := artc.CreateTime.Date()
	for i, archive := range Ei.Archives {
		if ay, am, _ := archive.Time.Date(); y == ay && m == am {
			Ei.Archives[i].Articles = append(Ei.Archives[i].Articles, artc)
			if needSort {
				sort.Sort(Ei.Archives[i].Articles)
				Ei.CH <- ARCHIVE_MD
			}
			return
		}
	}
	Ei.Archives = append(Ei.Archives, &Archive{Time: artc.CreateTime,
		Articles: SortArticles{artc}})
	if needSort {
		Ei.CH <- ARCHIVE_MD
	}
}

// 删除文章从tag、serie、archive
func dropArticle(artc *Article) {
	// tag
	for _, tag := range artc.Tags {
		for i, v := range Ei.Tags[tag] {
			if v == artc {
				Ei.Tags[tag] = append(Ei.Tags[tag][0:i], Ei.Tags[tag][i+1:]...)
				if len(Ei.Tags[tag]) == 0 {
					delete(Ei.Tags, tag)
				}
			}
		}
	}
	// serie
	for i, serie := range Ei.Series {
		if serie.ID == artc.SerieID {
			for j, v := range serie.Articles {
				if v == artc {
					Ei.Series[i].Articles = append(Ei.Series[i].Articles[0:j],
						Ei.Series[i].Articles[j+1:]...)
					Ei.CH <- SERIES_MD
					break
				}
			}
		}
	}
	// archive
	for i, archive := range Ei.Archives {
		ay, am, _ := archive.Time.Date()
		if y, m, _ := artc.CreateTime.Date(); ay == y && am == m {
			for j, v := range archive.Articles {
				if v == artc {
					Ei.Archives[i].Articles = append(Ei.Archives[i].Articles[0:j],
						Ei.Archives[i].Articles[j+1:]...)
					if len(Ei.Archives[i].Articles) == 0 {
						Ei.Archives = append(Ei.Archives[:i], Ei.Archives[i+1:]...)
					}
					Ei.CH <- ARCHIVE_MD
					break
				}
			}
		}
	}
}

// 替换文章
func ReplaceArticle(oldArtc *Article, newArtc *Article) {
	if oldArtc != nil {
		i, artc := GetArticle(oldArtc.ID)
		DelFromLinkedList(artc)
		Ei.Articles = append(Ei.Articles[:i], Ei.Articles[i+1:]...)
		delete(Ei.MapArticles, artc.Slug)

		dropArticle(oldArtc)
	}

	Ei.MapArticles[newArtc.Slug] = newArtc
	Ei.Articles = append(Ei.Articles, newArtc)
	sort.Sort(Ei.Articles)

	GenerateExcerptAndRender(newArtc)
	AddToLinkedList(newArtc.ID)

	upArticle(newArtc, true)
}

// 添加文章
func AddArticle(artc *Article) error {
	// 分配ID, 占位至起始id
	for {
		if id := node.Generate().Int64(); id < setting.Conf.General.StartID {
			continue
		} else {
			artc.ID = id
			break
		}
	}

	_, err := r.Table(COLLECTION_ARTICLE).Insert(artc).Run(session)
	if err != nil {
		return err
	}

	// 正式发布文章
	if !artc.IsDraft {
		defer GenerateExcerptAndRender(artc)
		Ei.MapArticles[artc.Slug] = artc
		Ei.Articles = append([]*Article{artc}, Ei.Articles...)
		sort.Sort(Ei.Articles)
		AddToLinkedList(artc.ID)

		upArticle(artc, true)
	}
	return nil
}

// 删除文章，移入回收箱
func DelArticles(ids ...int64) error {
	lock.Lock()
	defer lock.Unlock()
	for _, id := range ids {
		i, artc := GetArticle(id)
		DelFromLinkedList(artc)
		Ei.Articles = append(Ei.Articles[:i], Ei.Articles[i+1:]...)
		delete(Ei.MapArticles, artc.Slug)
		_, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"id": id}).Update(map[string]interface{}{"deletetime": time.Now()}).Run(session)
		if err != nil {
			return err
		}
		dropArticle(artc)
	}
	return nil
}

// 从链表里删除文章
func DelFromLinkedList(artc *Article) {
	if artc.Prev == nil && artc.Next != nil {
		artc.Next.Prev = nil
	} else if artc.Prev != nil && artc.Next == nil {
		artc.Prev.Next = nil
	} else if artc.Prev != nil && artc.Next != nil {
		artc.Prev.Next = artc.Next
		artc.Next.Prev = artc.Prev
	}
}

// 将文章添加到链表
func AddToLinkedList(id int64) {
	i, artc := GetArticle(id)
	if i == 0 && Ei.Articles[i+1].ID >= setting.Conf.General.StartID {
		artc.Next = Ei.Articles[i+1]
		Ei.Articles[i+1].Prev = artc
	} else if i > 0 && Ei.Articles[i-1].ID >= setting.Conf.General.StartID {
		artc.Prev = Ei.Articles[i-1]
		if Ei.Articles[i-1].Next != nil {
			artc.Next = Ei.Articles[i-1].Next
			Ei.Articles[i-1].Next.Prev = artc
		}
		Ei.Articles[i-1].Next = artc
	}
}

// 从缓存获取文章
func GetArticle(id int64) (int, *Article) {
	for i, artc := range Ei.Articles {
		if id == artc.ID {
			return i, artc
		}
	}
	return -1, nil
}

// 定时清除回收箱文章
func timer() {
	delT := time.NewTicker(time.Duration(setting.Conf.General.Clean) * time.Hour)
	for {
		<-delT.C
		r.Table(COLLECTION_ARTICLE).Filter(r.Row.Field("deletetime").Between(time.Time{},
			time.Now().Add(time.Duration(setting.Conf.General.Trash)*time.Hour))).Delete().Run(session)
	}
}

// 操作帐号字段
func UpdateAccountField(M map[string]interface{}) error {
	_, err := r.Table(COLLECTION_ACCOUNT).Filter(map[string]interface{}{"username": Ei.Username}).Update(M).Run(session)
	return err
}

// 删除草稿箱或回收箱，永久删除
func RemoveArticle(id int64) error {
	_, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"id": id}).Delete().Run(session)
	return err
}

// 恢复删除文章到草稿箱
func RecoverArticle(id int64) error {
	_, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"id": id}).Update(map[string]interface{}{"deletetime": time.Time{}, "isdraft": true}).Run(session)
	return err
}

// 更新文章
func UpdateArticle(id int64, update *Article) error {
	_, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"id": id}).Update(update).Run(session)
	return err
}

// 编辑文档
func QueryArticle(id int64) *Article {
	artc := &Article{}
	cur, err := r.Table(COLLECTION_ARTICLE).Filter(map[string]interface{}{"id": id}).Run(session)
	if err != nil {
		return nil
	}
	cur.One(&artc)
	return artc
}

// 添加专题
func AddSerie(name, slug, desc string) error {
	serie := &Serie{node.Generate().Int64(), name, slug, desc, time.Now(), nil}
	Ei.Series = append(Ei.Series, serie)
	sort.Sort(Ei.Series)
	Ei.CH <- SERIES_MD
	return UpdateAccountField(map[string]interface{}{"blogger": map[string]interface{}{"series": serie}})
}

// 更新专题
func UpdateSerie(serie *Serie) error {
	Ei.CH <- SERIES_MD
	_, err := r.Table(COLLECTION_ACCOUNT).Filter(map[string]interface{}{"username": Ei.Username,
		"blogger": map[string]interface{}{"series": map[string]interface{}{"id": serie.ID}}}).Update(map[string]interface{}{"blogger": map[string]interface{}{"series": serie}}).Run(session)
	return err
}

// 删除专题
func DelSerie(id int64) error {
	for i, serie := range Ei.Series {
		if id == serie.ID {
			if len(serie.Articles) > 0 {
				return fmt.Errorf("请删除该专题下的所有文章")
			}
			_, err := r.Table(COLLECTION_ACCOUNT).Filter(map[string]interface{}{"username": Ei.Username, "blogger": map[string]interface{}{"series": map[string]interface{}{"id": id}}}).Delete().Run(session)
			if err != nil {
				return err
			}
			Ei.Series[i] = nil
			Ei.Series = append(Ei.Series[:i], Ei.Series[i+1:]...)
			Ei.CH <- SERIES_MD
		}
	}
	return nil
}

// 查找专题
func QuerySerie(id int64) *Serie {
	for _, serie := range Ei.Series {
		if serie.ID == id {
			return serie
		}
	}
	return nil
}

// 后台分页
func PageListBack(se int, kw string, draft, del bool, p, n int) (max int, artcs []*Article) {
	T := r.Table(COLLECTION_ARTICLE)
	if draft {
		T = T.Filter(map[string]interface{}{"isdraft": true})
	} else if del {
		T = T.Ne(r.Row.Field("deletetime"), time.Time{})
	} else {
		T = T.Filter(map[string]interface{}{"isdraft": false}).Filter(map[string]interface{}{"deletetime": time.Time{}})
		if se > 0 {
			T = T.Filter(map[string]interface{}{"serieid": se})
		}
		if kw != "" {
			T = T.Filter(r.Row.Field("title").Match(kw))
		}
	}
	cur, err := T.Filter(map[string]interface{}{"content": 0}).OrderBy(r.Desc("createtime")).Limit(n).Skip((p - 1) * n).Run(session)
	if err != nil {
		logd.Error(err)
	}
	cur.All(&artcs)
	cur, err = T.Count().Run(session)
	var count int
	cur.One(&count)
	if err != nil {
		logd.Error(err)
	}
	max = count / n
	if count%n > 0 {
		max++
	}
	return
}
