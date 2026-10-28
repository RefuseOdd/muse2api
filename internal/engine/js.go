package engine

// The JavaScript below is the proven muse.ai DOM contract. Selectors keep the
// Chinese labels used by the live UI where necessary; everything else is
// English.

const waitReadyJS = `(function(){
    if (document.readyState !== 'complete') return 'loading';
    if (!document.querySelector('textarea')) return 'no-ta';
    var h = document.querySelector('[data-hatch-shell-hydration-state]');
    if (h && h.getAttribute('data-hatch-shell-hydration-state') !== 'hydrated') return 'hydrating';
    var b = document.body ? (document.body.innerText || '') : '';
    if (b.indexOf('Connecting...') !== -1) return 'connecting';
    if (document.querySelector('textarea').disabled) return 'input-disabled';
    return 'ready';
})()`

const resetThreadJS = `(function(forChat){
    var d = document.querySelector('[role="dialog"]');
    if (d) {
        var b = d.querySelector('button[aria-label*="close" i], button');
        if (b) b.click();
    }
    var scope = document.querySelector('main,[class*="chat-scroll"],[class*="hatch-chat-scroll"]') || document.body;
    var bubbleCount = scope ? scope.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]').length : 0;
    var hasAtts = document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"]').length > 0;
    var hasStop = !!(document.querySelector('[data-testid="hatch-composer-stop-button"]')
        || document.querySelector('button[aria-label*="Stop" i]')
        || document.querySelector('button[aria-label*="停止"]'));
    var bodyTxt = document.body ? (document.body.innerText || '') : '';
    var hasStuck = bodyTxt.indexOf('Still sending') !== -1 || bodyTxt.indexOf('Connecting...') !== -1;
    if (hasStop || hasStuck || hasAtts) return true;
    if (forChat) {
        return bubbleCount >= 24;
    }
    return (window.location.pathname !== '/thread/new') || bubbleCount > 0;
})(%s)`

const scrollBottomJS = `(function(){
    var els=[...document.querySelectorAll('*')].filter(function(e){
        var s=getComputedStyle(e);
        return (s.overflowY==='auto'||s.overflowY==='scroll')&&e.scrollHeight>e.clientHeight+100;});
    els.sort(function(a,b){return b.scrollHeight-a.scrollHeight;});
    if(els[0])els[0].scrollTop=els[0].scrollHeight;
    var s=document.scrollingElement||document.body;
    s.scrollTop=s.scrollHeight;
    var el=document.querySelector('textarea');
    if(el)el.scrollIntoView({block:'end'});return 1;})()`

const attachmentsJS = `(function(){
    var list = []; var seen = new Set();
    function addEl(el, tid){
      if(!el || seen.has(el)) return;
      seen.add(el);
      if(el.closest('form, [class*=chat-user-bubble], [class*="group/msg"]')) return;
      var v = el.querySelector('video') || (el.tagName === 'VIDEO' ? el : null);
      var img = el.querySelector('img') || (el.tagName === 'IMG' ? el : null);
      var isVid = (tid || '').includes('video') || !!v;
      var primary = isVid ? (v || img) : (img || v);
      var src = primary ? (primary.currentSrc || primary.src || '') : '';
      if(src && !seen.has(src)){
        seen.add(src);
        list.push({
          tid: tid || el.getAttribute('data-testid') || (isVid ? 'video' : 'image'),
          hasVideo: !!v,
          hasImg: !!img,
          src: src,
          vSrc: v ? (v.currentSrc || v.src || '') : '',
          iSrc: img ? (img.currentSrc || img.src || '') : '',
          w: primary ? (primary.videoWidth || primary.naturalWidth || 0) : 0,
          h: primary ? (primary.videoHeight || primary.naturalHeight || 0) : 0
        });
      }
    }
    document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"]').forEach(function(a){ addEl(a, a.getAttribute('data-testid')); });
    document.querySelectorAll('div[class*="hatch-agent-bubble-bg"] img, div[class*="hatch-agent-bubble-bg"] video').forEach(function(m){
      var s = m.currentSrc || m.src || '';
      if(s && !s.includes('avatar') && !s.includes('emoji')) addEl(m.parentElement || m, 'agent-media');
    });
    return JSON.stringify(list);
})()`

const focusTextareaJS = `(function(){
    var ta = document.querySelector('textarea');
    if (ta) {
        ta.scrollIntoView({block: 'center', inline: 'nearest'});
        ta.focus();
    }
})()`

const textareaRectJS = `(function(){var t=document.querySelector('textarea');if(!t)return null;
    var r=t.getBoundingClientRect();
    return JSON.stringify({x:Math.round(r.left+r.width/2),
    y:Math.round(r.top+r.height/2)});})()`

const setTextareaValueJS = `(function(t){var ta=document.querySelector('textarea');
    if(!ta) return 0;
    ta.focus();
    var s=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set;
    s.call(ta,t);
    ta.dispatchEvent(new Event('input',{bubbles:true}));
    ta.dispatchEvent(new Event('change',{bubbles:true}));
    return (ta.value||'').length;})(%s)`

const clickSendJS = `(function(){var b=[...document.querySelectorAll('button,[role=button]')]
    .filter(function(x){return x.offsetParent!==null;})
    .find(function(x){return /发送|send/i.test(x.getAttribute('aria-label')||'')
    ||/发送|send/i.test(x.getAttribute('data-testid')||'')
    ||/send/i.test(x.innerText||'');});
    if(!b)return 'no-button';
    if(b.disabled)return 'disabled';
    b.click();return 'clicked';})()`

const textareaEmptyJS = `!(document.querySelector('textarea')||{value:''}).value`

const pollChatJS = `(function(){
    var els=[...document.querySelectorAll('*')].filter(function(e){
        var s=getComputedStyle(e);
        return (s.overflowY==='auto'||s.overflowY==='scroll')&&e.scrollHeight>e.clientHeight+100;});
    els.sort(function(a,b){return b.scrollHeight-a.scrollHeight;});
    if(els[0])els[0].scrollTop=els[0].scrollHeight;
    var scope=document.querySelector('main,[class*="chat-scroll"],[class*="hatch-chat-scroll"]')||document.body;
    var bs=[].slice.call(scope.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'))
    .filter(function(b){return /hatch-agent-bubble-bg/.test(b.className||'');});
    var nonEmpty=bs.filter(function(b){return ((b.innerText||'').trim().length)>0;});
    var txt=nonEmpty.length?(nonEmpty[nonEmpty.length-1].innerText||'').trim():'';
    var stop=!!(document.querySelector('[data-testid="hatch-composer-stop-button"]')
    ||document.querySelector('button[aria-label*="Stop" i]')
    ||document.querySelector('button[aria-label*="停止"]'));
    return JSON.stringify({cnt:nonEmpty.length,total:bs.length,txt:txt,stop:stop});})()`

const agentTextJS = `(function(){
    var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'));
    for(var i=bs.length-1;i>=0;i--){
    var cs=bs[i].className||'';
    if(/hatch-agent-bubble-bg/.test(cs))return bs[i].innerText||'';}
    return '';})()`

const agentCountJS = `(function(){
    var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'));
    var n=0;for(var i=0;i<bs.length;i++){
    if(/hatch-agent-bubble-bg/.test(bs[i].className||''))n++;}
    return String(n);})()`

const waitStateJS = `(function(){
    var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'))
        .filter(function(b){return /hatch-agent-bubble-bg/.test(b.className||'');});
    var lastTxt = bs.length ? (bs[bs.length-1].innerText||'').trim() : '';
    var hasStop = !!(document.querySelector('[data-testid="hatch-composer-stop-button"]')
        || document.querySelector('button[aria-label*="Stop" i]')
        || document.querySelector('button[aria-label*="停止"]'));
    var tail = document.body ? (document.body.innerText||'').slice(-1200) : '';
    return JSON.stringify({cnt: bs.length, txt: lastTxt, stop: hasStop, tail: tail,
        path: location.pathname || '', href: location.href || ''});
})()`

const extractBytesJS = `(async function(src, expect){
  try{
    var u = src;
    if(!u) return JSON.stringify({ok:false,err:'no-media-src'});
    var r = await fetch(u);
    if(!r.ok) return JSON.stringify({ok:false,err:'media-http-'+r.status});
    var b = await r.blob();
    var ab = await b.arrayBuffer();
    var bytes = new Uint8Array(ab);
    var s = '';
    for(var i=0; i<bytes.length; i+=65536){
      s += String.fromCharCode.apply(null, bytes.subarray(i, i+65536));
    }
    return JSON.stringify({ok:true, mime:b.type||'', size:b.size, url:u, b64:btoa(s)});
  }catch(e){
    return JSON.stringify({ok:false, err:String(e)});
  }
})(%s, %s)`

const downloadScopedJS = `(function(src){
    var media=[...document.querySelectorAll('img,video')]
        .find(m=>(m.currentSrc||m.src||'')===src);
    var node=media && media.closest('[data-testid^="hatch-chat-attachment-presentation-"]');
    if(!node) return 'none';
    node=node.closest('[class*="group/widget-presentation"]')||node;
    var b=[...node.querySelectorAll('button,[role=button]')]
        .find(x=>/下载|保存|download/i.test(x.getAttribute('aria-label')||''));
    if(!b) return 'none';
    b.click();return 'ok';
})(%s)`

const clearAttachmentsJS = `(function(){
    var btns=Array.from(document.querySelectorAll('button[aria-label]')).filter(function(b){
        var al=b.getAttribute('aria-label')||'';
        return /remove\s*attachment|移除附件|删除附件|移除|删除/i.test(al);
    });
    btns.forEach(function(b){b.click();});
    Array.from(document.querySelectorAll('input[type="file"]')).forEach(function(inp){inp.value='';});
    return btns.length;
})()`

const attachImagesJS = `
(async function(images) {
    try {
        var inputs = Array.from(document.querySelectorAll('input[type="file"]'));
        if (!inputs.length) return JSON.stringify({ok: false, err: 'no-file-input'});
        var ov = document.querySelector('[data-testid="hatch-composer-placeholder-overlay"]');
        var composer = ov ? ov.closest('form') : null;
        var input = null;
        if (composer) {
            input = inputs.find(function(x){ return composer.contains(x); }) || null;
        }
        if (!input) {
            input = inputs.find(function(x){
                return !x.closest('[class*=chat-user-bubble], [class*="group/msg"]');
            }) || inputs[0];
        }
        if (!input.getAttribute('accept')) input.setAttribute('accept', 'image/*');
        for (var image of images) {
            var byteChars = atob(image.b64);
            var byteNumbers = new Array(byteChars.length);
            for (var i = 0; i < byteChars.length; i++) {
                byteNumbers[i] = byteChars.charCodeAt(i);
            }
            var mime = image.mime || 'image/png';
            var byteArray = new Uint8Array(byteNumbers);
            var blob = new Blob([byteArray], {type: mime});
            var ext = mime.split('/')[1] || 'png';
            if (ext === 'jpeg') ext = 'jpg';
            var dt = new DataTransfer();
            dt.items.add(new File(
                [blob], 'reference_image_' + image.index + '.' + ext, {type: mime}));
            input.files = dt.files;
            input.dispatchEvent(new Event('change', {bubbles: true}));
            input.dispatchEvent(new Event('input', {bubbles: true}));
            await new Promise(function(resolve){ setTimeout(resolve, 350); });
        }
        return JSON.stringify({ok: true, count: images.length});
    } catch(e) {
        return JSON.stringify({ok: false, err: String(e)});
    }
})(%s)`

const composerAttachedCountJS = `(function(){
    var ov = document.querySelector('[data-testid="hatch-composer-placeholder-overlay"]');
    var composer = ov ? (ov.closest('form') || ov.parentElement.parentElement.parentElement) : null;
    if (!composer) return 0;
    var btns = Array.from(composer.querySelectorAll('button[aria-label]'));
    var removeCount = btns.filter(function(b){
        return /remove\s*attachment|移除附件|删除附件/i.test(b.getAttribute('aria-label') || '');
    }).length;
    if (removeCount) return removeCount;
    return composer.querySelectorAll('img, video').length;
})()`

const quotaSettingsJS = `(() => {
    const visible = e => e.getClientRects().length > 0
        && getComputedStyle(e).visibility !== 'hidden';
    const labels = ['Settings', '設定', '设置'];
    const b = [...document.querySelectorAll('[data-testid="hatch-dock-more"]')].find(visible)
        || [...document.querySelectorAll('button,[role=button]')].find(e =>
            visible(e) && labels.includes((e.getAttribute('aria-label') || '').trim()));
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return JSON.stringify({x:Math.round(r.x+r.width/2),y:Math.round(r.y+r.height/2)});
})()`

const quotaMenuJS = `(() => {
    const visible = e => e.getClientRects().length > 0
        && getComputedStyle(e).visibility !== 'hidden';
    const labels = ['Settings', '設定', '设置'];
    const exact = e => labels.includes((e.textContent || '').trim());
    const b = [...document.querySelectorAll('[role=menuitem]')].find(e =>
        visible(e) && (exact(e) || [...e.querySelectorAll('span,div')].some(exact)))
        || [...document.querySelectorAll('[role=menu] button,[role=menu] span,[role=menu] div')]
            .find(e => visible(e) && exact(e));
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return JSON.stringify({x:Math.round(r.x+r.width/2),y:Math.round(r.y+r.height/2)});
})()`

const quotaDialogJS = `(() => {
    const d = [...document.querySelectorAll('[role=dialog],[aria-modal=true]')]
        .find(e => e.getClientRects().length > 0 && getComputedStyle(e).visibility !== 'hidden');
    return d ? (d.innerText || '') : '';
})()`

const debugDumpJS = `JSON.stringify({atts:[...document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"]')]
    .map(function(a){var m=a.querySelector('img,video');return {
    tid:a.getAttribute('data-testid'),
    src:m?(m.currentSrc||m.src||'').slice(0,60):''};}),
    buttons:[...document.querySelectorAll('button,[role=button]')]
    .filter(b=>b.offsetParent!==null)
    .map(b=>b.getAttribute('aria-label')||b.innerText.trim().slice(0,20))
    .filter(Boolean).slice(-40),
    tail:document.body.innerText.slice(-500)})`
