# coding:utf-8

# ---------------------------------------------------------------------------------
# MW-Linux面板
# ---------------------------------------------------------------------------------
# copyright (c) 2018-∞(https://github.com/midoks/mdserver-web) All rights reserved.
# ---------------------------------------------------------------------------------
# Author: midoks <midoks@163.com>
# ---------------------------------------------------------------------------------

import os
import sys
import re
import time
import math
import psutil
import json
import html

import core.mw as mw

PANEL_REPO_OWNER = 'AndyXeCM'
PANEL_REPO_NAME = 'PowerLinux'
PANEL_RELEASE_API = 'https://api.github.com/repos/%s/%s/releases/latest' % (PANEL_REPO_OWNER, PANEL_REPO_NAME)
PANEL_RELEASE_PAGE = 'https://github.com/%s/%s/releases/latest' % (PANEL_REPO_OWNER, PANEL_REPO_NAME)


def _normalize_version_name(version):
    version = str(version or '').strip()
    if version.startswith('v') and len(version) > 1 and version[1].isdigit():
        version = version[1:]
    return version


def _strip_html_tags(content):
    content = re.sub(r'(?i)<br\s*/?>', '\n', content)
    content = re.sub(r'(?i)</p\s*>', '\n\n', content)
    content = re.sub(r'(?i)</div\s*>', '\n', content)
    content = re.sub(r'(?i)</li\s*>', '\n', content)
    content = re.sub(r'(?i)<li[^>]*>', ' - ', content)
    content = re.sub(r'<[^>]+>', '', content)
    return html.unescape(content).strip()


def _parse_release_page(result):
    tag_name = ''
    release_name = ''

    title_match = re.search(r'<title>(.*?)</title>', result, re.S | re.I)
    if title_match is not None:
        title = title_match.group(1)
        title = re.sub(r'\s*·\s*[^<]+$', '', title).strip()
        title = re.sub(r'(?i)^release\s+', '', title).strip()
        if title != '':
            release_name = title
            version_match = re.search(r'(\d+(?:\.\d+)+)\s*$', release_name)
            if version_match is not None:
                tag_name = version_match.group(1)

    if tag_name == '':
        tag_candidates = re.findall(r'/[^/]+/[^/]+/releases/tag/([^"\'?#/]+)', result)
        for candidate in tag_candidates:
            candidate = _normalize_version_name(candidate)
            if re.match(r'^\d+(?:\.\d+)+$', candidate) is not None:
                tag_name = candidate
                break

    if release_name == '':
        release_name = tag_name

    if tag_name == '':
        return None

    body_match = re.search(r'<div[^>]+class="[^"]*markdown-body[^"]*"[^>]*>(.*?)</div>', result, re.S | re.I)
    body = ''
    if body_match is not None:
        body = _strip_html_tags(body_match.group(1))

    return {
        'tag_name': tag_name,
        'name': release_name,
        'body': body,
        'html_url': 'https://github.com/%s/%s/releases/tag/%s' % (PANEL_REPO_OWNER, PANEL_REPO_NAME, tag_name),
    }


def _release_version(version_new_info):
    version = _normalize_version_name(version_new_info.get('tag_name', ''))
    if version == '':
        version = _normalize_version_name(version_new_info.get('version', ''))
    if version == '':
        version = _normalize_version_name(version_new_info.get('name', ''))
        match = re.search(r'(\d+(?:\.\d+)+)', version)
        if match is not None:
            version = match.group(1)
    return version

def versionDiff(now, new):
    '''
        test 测试
        new 有新版本
        none 没有新版本
    '''
    now = _normalize_version_name(now)
    new = _normalize_version_name(new)
    if now == '' or new == '':
        return 'none'

    new_list = new.split('.')
    if len(new_list) > 3:
        return 'test'

    now_list = now.split('.')
    ret = 'none'
    from distutils.version import LooseVersion
    if LooseVersion(new) > LooseVersion(now):
        return 'new'
    else:
        return 'none'

def getServerInfo():
    import urllib.request
    import ssl
    headers = {
        'User-Agent': 'Folio-Updater/1.0',
        'Accept': 'application/vnd.github+json',
    }
    upAddrList = [PANEL_RELEASE_API, PANEL_RELEASE_PAGE]
    last_error = None
    try:
        context = ssl._create_unverified_context()
        for upAddr in upAddrList:
            try:
                req = urllib.request.Request(upAddr, headers=headers)
                resp = urllib.request.urlopen(req, context=context, timeout=5)
                result = resp.read().decode('utf-8')
                if upAddr == PANEL_RELEASE_API:
                    version = json.loads(result)
                    if isinstance(version, dict) and version.get('tag_name'):
                        return version
                else:
                    version = _parse_release_page(result)
                    if version is not None:
                        return version
            except Exception as e:
                last_error = e
    except Exception as e:
        print(str(e))
        return None
    if last_error is not None:
        print(str(last_error))
    return None

def updateServer(stype, version=''):
    import config
    # 更新服务
    try:
        if not mw.isRestart():
            return mw.returnData(False, '请等待所有安装任务完成再执行!')

        version_new_info = getServerInfo()
        if version_new_info is None:
            return mw.returnData(False, '服务器数据或网络有问题!')

        version_now = _normalize_version_name(config.APP_VERSION)
        new_ver = _release_version(version_new_info)
        if stype == 'check':
            diff = versionDiff(version_now, new_ver)
            if diff == 'new':
                return mw.returnData(True, '有新版本!', new_ver)
            elif diff == 'test':
                return mw.returnData(True, '有测试版本!', new_ver)
            else:
                return mw.returnData(False, '已经是最新,无需更新!')

        if stype == 'info':
            diff = versionDiff(version_now, new_ver)
            data = {}
            data['version'] = new_ver
            content = str(version_new_info.get('body', ''))
            if content.strip() == '':
                content = '当前版本来自 GitHub Release，未提供更新说明。'
            data['content'] = content.replace("\n", "<br/>")
            return mw.returnData(True, '更新信息!', data)

        if stype == 'update':
            if version == '':
                return mw.returnData(False, '缺少版本信息!')

            if new_ver != version:
                return mw.returnData(False, '更新失败,请重试!')

            toPath = mw.getPanelDir() + '/temp'
            if not os.path.exists(toPath):
                mw.execShell('mkdir -p ' + toPath)

            version = _normalize_version_name(version)
            newUrl = "https://github.com/%s/%s/archive/refs/tags/%s.zip" % (PANEL_REPO_OWNER, PANEL_REPO_NAME, version)

            dist_mw = toPath + '/mw.zip'
            if not os.path.exists(dist_mw):
                mw.execShell('wget --no-check-certificate -O ' + dist_mw + ' ' + newUrl)

            dist_to = toPath + "/PowerLinux-" + version
            if not os.path.exists(dist_to):
                os.system('unzip -o ' + toPath + '/mw.zip' + ' -d ' + toPath)

            cmd_cp = 'cp -rf ' + toPath + '/PowerLinux-' + version + '/* ' + mw.getServerDir() + '/mdserver-web'
            mw.execShell(cmd_cp)

            mw.execShell('rm -rf ' + toPath + '/PowerLinux-' + version)
            mw.execShell('rm -rf ' + toPath + '/mw.zip')

            update_env = '''
#!/bin/bash
PATH=/bin:/sbin:/usr/bin:/usr/sbin:/usr/local/bin:/usr/local/sbin:~/bin

P_VER=`python3 -V | awk '{print $2}'`

if [ ! -f /www/server/mdserver-web/bin/activate ];then
cd /www/server/mdserver-web && python3 -m venv .
cd /www/server/mdserver-web && source /www/server/mdserver-web/bin/activate
else
cd /www/server/mdserver-web && source /www/server/mdserver-web/bin/activate
fi

cn=$(curl -fsSL -m 10 http://ipinfo.io/json | grep "\"country\": \"CN\"")
PIPSRC="https://pypi.python.org/simple"
if [ ! -z "$cn" ];then
PIPSRC="https://pypi.tuna.tsinghua.edu.cn/simple"
fi

cd /www/server/mdserver-web && pip3 install -r /www/server/mdserver-web/requirements.txt -i $PIPSRC

P_VER_D=`echo "$P_VER"|awk -F '.' '{print $1}'`
P_VER_M=`echo "$P_VER"|awk -F '.' '{print $2}'`
NEW_P_VER=${P_VER_D}.${P_VER_M}

if [ -f /www/server/mdserver-web/version/r${NEW_P_VER}.txt ];then
cd /www/server/mdserver-web && pip3 install -r /www/server/mdserver-web/version/r${NEW_P_VER}.txt -i $PIPSRC
fi
'''
            os.system(update_env)
            mw.restartMw()
            return mw.returnData(True, '安装更新成功!')

        return mw.returnData(False, '已经是最新,无需更新!')
    except Exception as ex:
        # print('updateServer', ex)
        return mw.returnData(False, "连接服务器失败!" + str(ex))

