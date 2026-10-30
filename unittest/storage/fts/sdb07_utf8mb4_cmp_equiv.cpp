// Copyright (c) 2026 OceanBase.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// SDB-07: exhaustive-random equivalence of the inline utf8mb4_general_ci prefix compare
// (ObDatumUtf8mb4GeneralCiCmp in src/share/datum/ob_datum_cmp_func_def.h) against
// the original ob_strnncollsp_utf8mb4, for both end-space modes.
#include "lib/charset/ob_ctype_utf8.cc"
#include <random>
#include <cstdio>
#include <vector>
#include <string>
using namespace std;
static bool simple_char(const unsigned char *p, long remain, long &clen, ob_wc_t &wc){
  const unsigned char c=p[0];
  if(c<0x80){clen=1;wc=c;return true;}
  if(c>=0xE4&&c<=0xE9&&remain>=3&&0x80==(p[1]&0xC0)&&0x80==(p[2]&0xC0)){clen=3;wc=((ob_wc_t)(c&0x0F)<<12)|((ob_wc_t)(p[1]&0x3F)<<6)|(ob_wc_t)(p[2]&0x3F);return true;}
  return false;
}
static void to_sort(const ObCharsetInfo &cs, ob_wc_t &wc){
  const ObUnicaseInfo *u=cs.caseinfo;
  if(wc<=u->maxchar){const ObUnicaseInfoChar *pg=u->page[wc>>8]; if(pg) wc=(cs.state&OB_CS_LOWER_SORT)?pg[wc&0xFF].tolower:pg[wc&0xFF].sort;} else wc=OB_CS_REPLACEMENT_CHARACTER;
}
static int norm(int x){return x>0?1:(x<0?-1:0);}
static int ref(const string&a,const string&b,bool es){
  const ObCharsetInfo*cs=&ob_charset_utf8mb4_general_ci;
  return norm(cs->coll->strnncollsp(cs,(const unsigned char*)a.data(),a.size(),(const unsigned char*)b.data(),b.size(),es));
}
static int mine(const string&a,const string&b,bool es){
  if(a.size()==b.size()&&a==b) return 0;
  const ObCharsetInfo*cs=&ob_charset_utf8mb4_general_ci;
  const unsigned char*s=(const unsigned char*)a.data(),*t=(const unsigned char*)b.data();
  long sl=a.size(),tl=b.size(),n=min(sl,tl),i=0;
  while(i<n){
    long ls=0,lt=0; ob_wc_t ws=0,wt=0;
    if(s[i]==t[i]&&s[i]<0x80){++i;}
    else if(!simple_char(s+i,sl-i,ls,ws)||!simple_char(t+i,tl-i,lt,wt)||ls!=lt) break;
    else if(ws==wt){i+=ls;}
    else {to_sort(*cs,ws);to_sort(*cs,wt); if(ws!=wt) return ws>wt?1:-1; i+=ls;}
  }
  return norm(cs->coll->strnncollsp(cs,s+i,sl-i,t+i,tl-i,es));
}
int main(){
  mt19937 rng(42);
  vector<string> atoms={"a","A","b","Z","z"," ","\t","0","_","\xE6\x95\xB0","\xE6\x8D\xAE","\xE5\xBA\x93","\xE4\xB8\x80","\xE9\xBF\xBF","\xE4\x80\x80","\xC3\xA9","\xC3\x89","\xEF\xBC\xA1","\xF0\x9F\x98\x80","\xE6\x95","\xE6","\x80","\xFF","\xED\xA0\x80","\xE0\x80\x80","\xE3\x80\x80","\xE9\xBF"};
  long bad=0,total=0;
  for(int it=0;it<3000000;++it){
    string a,b; int la=rng()%6, lb=rng()%6;
    for(int k=0;k<la;++k) a+=atoms[rng()%atoms.size()];
    if(rng()%2){ b=a.substr(0, a.empty()?0:rng()%(a.size()+1)); } 
    for(int k=0;k<lb;++k) b+=atoms[rng()%atoms.size()];
    if(rng()%3==0) swap(a,b);
    for(int es=0;es<2;++es){ ++total; if(ref(a,b,es)!=mine(a,b,es)){ if(bad<10) printf("MISMATCH es=%d ref=%d mine=%d\n",es,ref(a,b,es),mine(a,b,es)); ++bad; } }
  }
  printf("total=%ld bad=%ld\n",total,bad);
  return bad==0?0:1;
}
extern "C" void right_to_die_or_duty_to_live_c() { __builtin_trap(); }
