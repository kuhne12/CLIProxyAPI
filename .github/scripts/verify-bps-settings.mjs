const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
import { createServer } from 'node:http';
import { readFileSync, mkdirSync, writeFileSync } from 'node:fs';
import assert from 'node:assert/strict';
const root=process.cwd();
const dir=process.env.BPS_SETTINGS_ARTIFACT_DIR || '/tmp/bps-ultrafast-settings-verification';mkdirSync(dir,{recursive:true});
let config={'enabled':true,'cache-creation-as-input':true,'image-relay-origin':'https://cpa.example.test','force-ultrafast':false,'force-ultrafast-auth-ids':[], 'quota-model-prices':{'fixture-model':{input:2,output:10}}};
let revision=1, failPatch=false, failCredentials=false;
const writes=[],reads=[];
const server=createServer(async(req,res)=>{
 const url=new URL(req.url,'http://localhost');
 if(url.pathname==='/v0/resource/plugins/bps/settings'){res.setHeader('content-type','text/html');return res.end(readFileSync(root+'/examples/plugin/bps/go/web/settings.html'));}
 if(req.headers.authorization!=='Bearer fixture-key'){res.statusCode=401;return res.end('{}');}
 res.setHeader('content-type','application/json');
 if(req.method==='GET')reads.push(url.pathname);
 if(url.pathname==='/v8/management/config/plugins/configs/bps'){
  if(req.method==='PATCH'){
   let raw='';for await(const chunk of req)raw+=chunk;
   if(failPatch||req.headers['if-match']!==String(revision)){failPatch=false;res.statusCode=412;return res.end('{}');}
   const patch=JSON.parse(raw);writes.push(patch);config={...config,...patch};revision++;
  }
  res.setHeader('etag',String(revision));return res.end(JSON.stringify(config));
 }
 if(url.pathname==='/v8/management/credentials'){
  if(failCredentials){res.statusCode=503;return res.end('{}');}
  return res.end(JSON.stringify({files:[{id:'bps-one.json',name:'bps-one.json',provider:'bps'},{id:'bps-two.json',name:'bps-two.json',provider:'bps',disabled:true},{id:'codex-other.json',name:'codex-other.json',provider:'codex'},{id:'bps-<script>fixture</script>.json',name:'bps-<script>fixture</script>.json',provider:'bps'}]}));
 }
 if(url.pathname==='/v8/management/credentials/models')return res.end(JSON.stringify({models:[{id:'fixture-model'}]}));
 res.statusCode=404;res.end('{}');
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const origin='http://127.0.0.1:'+server.address().port;
const browser=await chromium.launch({channel:'chrome',headless:true});
const context=await browser.newContext({viewport:{width:1280,height:1000},locale:'zh-CN'});
await context.route('**/*',r=>r.request().url().startsWith(origin)?r.continue():r.abort());
const page=await context.newPage(),errors=[];page.on('pageerror',e=>errors.push(e.message));
const save=()=>page.getByRole('button',{name:'保存 Ultra fast 设置',exact:true});
const saved=()=>page.waitForFunction(()=>document.getElementById('status').textContent==='Ultra fast 设置已保存，仅对新请求生效。' && !document.getElementById('settings').disabled);
try {
 await page.goto(origin+'/v0/resource/plugins/bps/settings');
 await page.locator('#key').fill('fixture-key');await page.getByRole('button',{name:'读取设置',exact:true}).click();
 await page.locator('#settings:not([disabled])').waitFor();await save().waitFor();
 assert.equal(await page.locator('#ultrafast-mode').inputValue(),'off');
 assert.equal(await page.locator('#ultrafast-selection').isVisible(),false);
 await page.locator('#ultrafast-mode').selectOption('selected');
 assert.equal(await page.locator('#ultrafast-accounts input').count(),3);
 assert.equal(await page.locator('#ultrafast-accounts script').count(),0);
 await save().click();assert.equal(writes.length,0);
 assert.match(await page.locator('#status').innerText(),/至少选择/);
 await page.locator('#ultrafast-accounts input').filter({visible:true}).evaluateAll(xs=>{const x=xs.find(x=>x.value==='bps-one.json');x.click();});
 await save().click();await saved();
 assert.deepEqual(writes.at(-1),{'force-ultrafast':false,'force-ultrafast-auth-ids':['bps-one.json']});
 assert.equal(config['cache-creation-as-input'],true);assert.equal(config['image-relay-origin'],'https://cpa.example.test');
 await page.locator('#reload').click();await page.locator('#settings:not([disabled])').waitFor();
 assert.equal(await page.locator('#ultrafast-mode').inputValue(),'selected');
 assert.equal(await page.locator('#ultrafast-accounts input:checked').count(),1);
 await page.screenshot({path:dir+'/selected-light.png',fullPage:true});
 await page.locator('#ultrafast-mode').selectOption('all');await save().click();await saved();
 assert.equal(writes.at(-1)['force-ultrafast'],true);
 await page.locator('#ultrafast-mode').selectOption('off');await save().click();await saved();
 assert.deepEqual(writes.at(-1),{'force-ultrafast':false,'force-ultrafast-auth-ids':[]});
 const writeCount=writes.length;failPatch=true;
 await page.locator('#ultrafast-mode').selectOption('all');await save().click();
 await page.getByText('配置已被其他操作修改。请重新读取最新设置后再编辑。',{exact:true}).first().waitFor();
 assert.equal(writes.length,writeCount);await save().click();assert.equal(writes.length,writeCount);
 config['force-ultrafast-auth-ids']=['bps-unavailable.json'];revision++;failCredentials=true;
 await page.locator('#reload').click();await page.locator('#settings:not([disabled])').waitFor();
 assert.equal(await page.locator('#ultrafast-accounts input:checked').inputValue(),'bps-unavailable.json');
 assert.match(await page.locator('#accounts-status').innerText(),/已保留/);
 const switches=page.locator('#cooldown-enabled');
 const threshold=page.locator('#cooldown-threshold'), duration=page.locator('#cooldown-seconds');
 const saveCooldown=page.locator('#cooldown-form button');
 assert.equal(await switches.isChecked(),false);
 assert.equal(await threshold.inputValue(),'3');
 assert.equal(await duration.inputValue(),'60');
 await switches.check();await threshold.fill('4');await duration.fill('120');await saveCooldown.click();
 await page.locator('#status').filter({hasText:'账号冷却设置已保存。'}).waitFor();
 assert.deepEqual(writes.at(-1),{'rate-limit-cooldown-enabled':true,'rate-limit-cooldown-threshold':4,'rate-limit-cooldown-seconds':120});
 assert.deepEqual(config['force-ultrafast-auth-ids'],['bps-unavailable.json']);
 await page.locator('#reload').click();await page.locator('#settings:not([disabled])').waitFor();
 assert.equal(await switches.isChecked(),true);assert.equal(await threshold.inputValue(),'4');assert.equal(await duration.inputValue(),'120');
 await switches.uncheck();await saveCooldown.click();
 await page.locator('#status').filter({hasText:'账号冷却设置已保存。'}).waitFor();
 assert.equal(writes.at(-1)['rate-limit-cooldown-enabled'],false);
 await page.emulateMedia({colorScheme:'dark'});await page.setViewportSize({width:390,height:844});
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
 await page.screenshot({path:dir+'/selected-dark-mobile.png',fullPage:true});
 assert.deepEqual(errors,[]);
 const result={passed:true,checks:['off by default','BPS-only account options','escaped labels','empty selection blocked','exact selected ID saved','global mode','off clears force','unrelated settings preserved','ETag conflict safe','unavailable selections retained','390px dark layout','cooldown defaults','cooldown persists','disable cooldown'],writes:writes.length,credentialReads:reads.filter(p=>p.endsWith('/credentials')).length,errors};
 writeFileSync(dir+'/report.json',JSON.stringify(result,null,2));console.log(JSON.stringify(result));
}finally{await browser.close();await new Promise(r=>server.close(r));}
