#!/usr/bin/env python3
# 极简 ClusterIP 代理：沙箱内核缺少 xt_comment/xt_mark/xt_statistic，kube-proxy 无法运行。
# 每 5 秒读取 Service/EndpointSlice，用 iptables-legacy DNAT 把 ClusterIP:port 指向第一个就绪端点。
import json, subprocess, time
IPT="/var/lib/rancher/k3s/data/current/bin/aux/iptables-legacy"
K=["/workspace/tools/k3s/k3s","kubectl"]
def sh(a): return subprocess.run(a,capture_output=True,text=True)
def ipt(*a): return sh([IPT,"-t","nat",*a])
for ch in ("XS-SVC",):
    ipt("-N",ch)
    for hook in ("PREROUTING","OUTPUT"):
        if ipt("-C",hook,"-j",ch).returncode: ipt("-I",hook,"1","-j",ch)
last=None
while True:
    try:
        svcs=json.loads(sh(K+["get","svc","-A","-o","json"]).stdout)["items"]
        eps=json.loads(sh(K+["get","endpointslices","-A","-o","json"]).stdout)["items"]
        rules=[]
        for s in svcs:
            ip=s["spec"].get("clusterIP")
            if not ip or ip=="None": continue
            ns,name=s["metadata"]["namespace"],s["metadata"]["name"]
            for p in s["spec"].get("ports",[]):
                tgt=None
                for e in eps:
                    if e["metadata"]["namespace"]!=ns or e["metadata"].get("labels",{}).get("kubernetes.io/service-name")!=name: continue
                    port=next((ep["port"] for ep in e.get("ports") or [] if ep.get("name","")==p.get("name","")),None)
                    for en in e.get("endpoints") or []:
                        if en.get("conditions",{}).get("ready",True) and port:
                            tgt=f'{en["addresses"][0]}:{port}'; break
                    if tgt: break
                if tgt: rules.append((ip,p.get("protocol","TCP").lower(),p["port"],tgt))
        if rules!=last:
            ipt("-F","XS-SVC")
            for ip,proto,port,tgt in rules:
                ipt("-A","XS-SVC","-d",ip,"-p",proto,"--dport",str(port),"-j","DNAT","--to-destination",tgt)
            last=rules
    except Exception as ex: print(ex,flush=True)
    time.sleep(5)
