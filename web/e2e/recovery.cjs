const { chromium }=require(process.env.TEST_RECOVERY_PLAYWRIGHT || 'playwright');
(async()=>{
 let input='';for await(const b of process.stdin)input+=b;const cfg=JSON.parse(input);
 const browser=await chromium.launch({headless:true});let phase='login';
 try{
  const ctx=await browser.newContext();const page=await ctx.newPage();
  const login=async(p,password)=>{await p.goto(cfg.base+'/login');await p.getByLabel('Account',{exact:true}).fill(cfg.subject);await p.getByLabel('Password',{exact:true}).fill(password);await p.getByRole('button',{name:'Sign in',exact:true}).click();await p.waitForURL('**/overview')};
  await login(page,cfg.credential);
  const other=await browser.newContext();const otherPage=await other.newPage();await login(otherPage,cfg.credential);
  phase='mailbox enrollment';await page.goto(cfg.base+'/mailbox');await page.getByLabel('Email address',{exact:true}).fill('person@example.test');await page.getByLabel('Current password',{exact:true}).fill(cfg.credential);await page.getByRole('button',{name:'Request verification',exact:true}).click();await page.getByRole('status').filter({hasText:'Verification requested'}).waitFor();
  const mailLink=async()=>{const r=await fetch(cfg.inbox);if(r.status!==200)throw Error();const {message}=await r.json();const m=message.match(/https:\/\/kanz\.test\/[^\s]+/);if(!m)throw Error();return new URL(m[0])};
  phase='mailbox proof';const verify=await mailLink();await page.goto(cfg.base+verify.pathname+verify.hash);await page.getByRole('button',{name:'Verify mailbox',exact:true}).click();await page.getByRole('status').filter({hasText:'Recovery mailbox verified'}).waitFor();if(new URL(page.url()).hash)throw Error();
  await page.goto(cfg.base+'/mailbox');await page.getByText(/Verified address: person@example.test/).waitFor();
  phase='recovery request';await page.goto(cfg.base+'/recover');await page.getByLabel('Account',{exact:true}).fill(cfg.subject);await page.getByRole('button',{name:'Request recovery link',exact:true}).click();await page.getByRole('status').filter({hasText:'If this account'}).waitFor();
  phase='reset';const reset=await mailLink();const token=new URLSearchParams(reset.hash.slice(1)).get('token');await page.goto(cfg.base+reset.pathname+reset.hash);await page.getByLabel('New password',{exact:true}).fill(cfg.replacement);await page.getByLabel('Confirm new password',{exact:true}).fill(cfg.replacement);await page.getByRole('button',{name:'Reset password',exact:true}).click();await page.getByRole('status').filter({hasText:'Password reset'}).waitFor();
  phase='revocation and replay';const old=await other.request.get(cfg.base+'/auth/mailbox');if(old.status()!==401)throw Error();
  const replay=await ctx.request.post(cfg.base+'/auth/recovery/consume',{headers:{Origin:cfg.base},data:{token,credential:cfg.replacement}});if(replay.status()!==401)throw Error();
  const oldPassword=await ctx.request.post(cfg.base+'/auth/login',{headers:{Origin:cfg.base},data:{subject:cfg.subject,credential:cfg.credential}});if(oldPassword.status()!==401)throw Error();
  phase='new credential';await login(page,cfg.replacement);
 }catch{console.error('Browser verification failed during '+phase);process.exitCode=1}finally{await browser.close()}
})().catch(()=>{console.error('Browser harness startup failed');process.exitCode=1});
