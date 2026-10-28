package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/cdp"
)

func runVerifySelectors(args []string) int {
	fs := flag.NewFlagSet("verify-selectors", flag.ContinueOnError)
	port := fs.Int("port", 19210, "DevTools port of the running browser")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	targets, err := devtoolsTargets(*port)
	if err != nil {
		fmt.Println("Could not list DevTools targets:", err)
		return 1
	}
	wsURL := ""
	pageURL := ""
	for _, target := range targets {
		typ, _ := target["type"].(string)
		url, _ := target["url"].(string)
		if typ == "page" && strings.Contains(url, "muse.ai") {
			wsURL, _ = target["webSocketDebuggerUrl"].(string)
			pageURL = url
			break
		}
	}
	if wsURL == "" {
		fmt.Println("No muse.ai page target was found.")
		return 1
	}
	fmt.Println("[verify] page =", pageURL)
	cli, err := cdp.Dial(wsURL, 30*time.Second)
	if err != nil {
		fmt.Println("Could not connect to the page:", err)
		return 1
	}
	defer cli.Close()

	labels, _ := cli.EvaluateString(`JSON.stringify(Array.from(document.querySelectorAll('button[aria-label]')).map(function(b){return b.getAttribute('aria-label');}))`, false, 20*time.Second)
	fmt.Println("\n=== button[aria-label] on page ===")
	fmt.Println(labels)

	attachJS := `JSON.stringify(Array.from(document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"]')).map(function(a){var m=a.querySelector('img,video');return {tid:a.getAttribute('data-testid'),src:m?(m.currentSrc||m.src||''):'',tag:m?m.tagName:''};}))`
	atts, _ := cli.EvaluateString(attachJS, false, 20*time.Second)
	fmt.Println("\n=== attachments ===")
	fmt.Println(atts)

	fmt.Println("\n=== injecting a 1x1 PNG into the composer ===")
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmM"
	payload, _ := json.Marshal(pngB64)
	inject := `(async function(b64){
        try{
            var bin=atob(b64); var arr=new Uint8Array(bin.length);
            for(var i=0;i<bin.length;i++) arr[i]=bin.charCodeAt(i);
            var blob=new Blob([arr],{type:'image/png'});
            var file=new File([blob],'reference_image.png',{type:'image/png'});
            var inputs=Array.from(document.querySelectorAll('input[type="file"]'));
            var ov=document.querySelector('[data-testid="hatch-composer-placeholder-overlay"]');
            var composer=ov?ov.closest('form'):null;
            var input=null;
            if(composer) input=inputs.find(function(x){return composer.contains(x);})||null;
            if(!input) input=inputs[0];
            if(!input) return JSON.stringify({ok:false,err:'no-file-input'});
            input.setAttribute('accept','image/*');
            var dt=new DataTransfer(); dt.items.add(file);
            input.files=dt.files;
            input.dispatchEvent(new Event('change',{bubbles:true}));
            input.dispatchEvent(new Event('input',{bubbles:true}));
            return JSON.stringify({ok:true});
        }catch(e){return JSON.stringify({ok:false,err:String(e)});}
    })(` + string(payload) + `)`
	result, _ := cli.EvaluateString(inject, true, 30*time.Second)
	fmt.Println("inject:", result)

	checkJS := `(function(){
        var ov=document.querySelector('[data-testid="hatch-composer-placeholder-overlay"]');
        var comp=ov?(ov.closest('form')||ov.parentElement.parentElement.parentElement):null;
        if(!comp) return JSON.stringify({remove:0,images:0});
        var remove=Array.from(comp.querySelectorAll('button[aria-label]')).filter(function(b){
            return /remove\s*attachment|移除附件|删除附件/i.test(b.getAttribute('aria-label')||'');
        }).length;
        return JSON.stringify({remove:remove,images:comp.querySelectorAll('img,video').length});
    })()`
	for i := 0; i < 12; i++ {
		time.Sleep(500 * time.Millisecond)
		state, _ := cli.EvaluateString(checkJS, false, 20*time.Second)
		fmt.Printf("  [t=%.1fs] %s\n", float64(i+1)*0.5, state)
		if strings.Contains(state, `"remove":1`) || strings.Contains(state, `"remove":`) && !strings.Contains(state, `"remove":0`) {
			break
		}
	}

	clearJS := `(function(){
        var btns=Array.from(document.querySelectorAll('button[aria-label]')).filter(function(b){
            var al=b.getAttribute('aria-label')||'';
            return /remove\s*attachment|移除附件|删除附件|移除|删除/i.test(al);
        });
        btns.forEach(function(b){b.click();});
        return btns.length;
    })()`
	cleared, _ := cli.EvaluateString(clearJS, false, 20*time.Second)
	fmt.Println("cleared:", cleared)
	return 0
}

func devtoolsTargets(port int) ([]map[string]any, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json", port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
