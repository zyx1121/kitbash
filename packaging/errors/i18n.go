package main

import (
	_ "embed"
	"encoding/json"
	"html/template"
)

//go:embed zh-TW.json
var chineseJSON []byte
var chinese = func() map[string]string {
	var messages map[string]string
	if err := json.Unmarshal(chineseJSON, &messages); err != nil {
		panic(err)
	}
	return messages
}()

func translate(locale string) func(string) string {
	return func(source string) string {
		if locale == "zh-TW" {
			if translated, ok := chinese[source]; ok {
				return translated
			}
		}
		return source
	}
}

func localeFuncs(locale string) template.FuncMap {
	t := translate(locale)
	return template.FuncMap{
		"t": t, "lang": func() string { return locale },
		"prefix": func() string {
			if locale == "en" {
				return "/en"
			}
			return ""
		},
		// Only repository-owned explanatory paragraphs can use this function.
		"rich":           func(text string) template.HTML { return template.HTML(t(text)) },
		"languageScript": func() template.JS { return template.JS(languageScript) },
	}
}

const languageScript = `(function(){const u=new URL(location.href),valid=v=>v==="en"||v==="zh-TW",explicit=u.searchParams.get("lang"),saved=document.cookie.split(";").map(x=>x.trim()).find(x=>x.startsWith("zyx_locale="))?.slice(11),english=u.pathname==="/en"||u.pathname.startsWith("/en/"),locale=valid(explicit)?explicit:english?"en":valid(saved)?saved:"zh-TW",save=lang=>{document.cookie="zyx_locale="+lang+"; Path=/; Max-Age=31536000; SameSite=Lax"+((location.hostname==="zyx.tw"||location.hostname.endsWith(".zyx.tw"))?"; Domain=.zyx.tw":"")+(location.protocol==="https:"?"; Secure":"")},path=english?u.pathname.slice(3)||"/":u.pathname;if(valid(explicit))save(locale);const target=(locale==="en"?"/en":"")+path;if(target!==u.pathname){u.pathname=target;location.replace(u.href)}addEventListener("DOMContentLoaded",()=>{const a=document.querySelector("[data-language]");if(!a)return;const next=locale==="en"?"zh-TW":"en",dest=new URL(location.href);dest.pathname=(next==="en"?"/en":"")+path;dest.searchParams.set("lang",next);a.href=dest.href;a.addEventListener("click",()=>save(next))})})();`
