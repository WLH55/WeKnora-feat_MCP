# -*- coding: utf-8 -*-
"""生成三分钟演示视频的口播音频（微软 Edge TTS，免费）。
每个镜头一个 MP3 + 对应字级 SRT，方便剪映逐镜头对齐。
用法：python gen_voiceover.py [语速，如 +8%]
"""
import asyncio
import sys
from pathlib import Path

import edge_tts

VOICE = "zh-CN-YunxiNeural"  # 年轻男声；备选 zh-CN-YunjianNeural(沉稳男声) / zh-CN-XiaoxiaoNeural(女声)
OUT = Path(__file__).resolve().parent

# (文件名, 时间预算秒, 口播文本)
SEGMENTS = [
    ("01-开场", 15,
     "我们团队的知识，散落在钉钉群、网盘和老员工的脑子里，找东西基本靠问人。"
     "我们把腾讯开源的 WeKnora 完整私有化部署在内网——所有数据都在自己的服务器上，不出内网。"),
    ("02-钉钉同步", 25,
     "WeKnora 官方支持飞书、Notion、语雀的数据源同步，但我们公司的主协作平台是钉钉。"
     "所以我们基于它的连接器框架，二次开发了钉钉数据源：支持钉钉知识库整库同步，也可以按链接同步单篇文档。"
     "现在钉钉里的文档，点一下同步就自动进入知识库，全程没有人工搬运。"),
    ("03-问答溯源", 20,
     "知识进来之后怎么用？直接在对话页提问。比如：这个报错之前是怎么解决的？"
     "它基于知识库检索回答，并且强制附带原文引用——点开就能看到出处。答案可溯源、不瞎编。"),
    ("04-MCP双向", 45,
     "更关键的是这一步：我们把 WeKnora 的 MCP 接口接进了本地 AI 编码工具，双向打通。"
     "先说沉淀：刚解决完一个线上问题，我直接跟 AI 说，把这次的排障经验沉淀进知识库——"
     "它自动调用工具写入，知识库里立刻多了这篇文档。"
     "再说检索：下次遇到类似问题，我在编码工具里直接问，AI 先查公司知识库再回答，方案和引用一次给到。"
     "知识沉淀在工作的动线上，而不是又一个要人维护的系统。"),
    ("05-按人APIKey", 25,
     "企业落地还差一道门：权限。原生 API 接入不区分人，谁拿到 Key 谁就有全部权限。"
     "所以我们二次开发了按人授权：每位同事一把独立 API Key，能力逐项勾选、知识库范围可限定。"
     "用新人的 Key 问核心架构决策，AI 直接拒绝——AI 和人同权，没有越权旁路。"),
    ("06-观测与收益", 25,
     "所有调用都有 Langfuse 全链路追踪，调用量、token 消耗一目了然，模型成本可算可控。"
     "目前只有我一个人在用：按实测口径，年化提效约一万一千五，已经覆盖服务器加模型 API 的全部运行成本；"
     "推广到全组五个人，净收益约每年四万二。"),
    ("07-收尾", 25,
     "总结一下：开源底座加两项二次开发，把散落的知识变成可检索、可溯源、带权限的公司资产——"
     "老员工少被打断，新人少走弯路，知识归公司，不随人走。谢谢观看。"),
]


async def synth(name: str, text: str, rate: str) -> float:
    mp3 = OUT / f"{name}.mp3"
    sub = edge_tts.SubMaker()
    for attempt in range(3):
        try:
            kwargs = {} if rate == "+0%" else {"rate": rate}
            com = edge_tts.Communicate(text, VOICE, **kwargs)
            with open(mp3, "wb") as f:
                async for chunk in com.stream():
                    if chunk["type"] == "audio":
                        f.write(chunk["data"])
                    elif chunk["type"] == "WordBoundary":
                        try:
                            sub.feed(chunk)
                        except Exception:
                            pass
            break
        except Exception as e:
            if attempt == 2:
                raise
            print(f"  ({name} 第{attempt + 1}次失败: {e}，重试…)")
            await asyncio.sleep(2)
    try:
        (OUT / f"{name}.srt").write_text(sub.get_srt(), encoding="utf-8-sig")
    except Exception as e:
        print(f"  ({name} SRT 跳过: {e})")
    # edge-tts 默认 24kHz/48kbps CBR → 6000 字节/秒
    return mp3.stat().st_size / 6000.0


async def main() -> None:
    rate = sys.argv[1] if len(sys.argv) > 1 else "+0%"
    print(f"voice={VOICE} rate={rate}\n")
    total = 0.0
    budget_total = 0
    print(f"{'镜头':<14}{'时长':>8}{'预算':>6}{'余量':>8}")
    for name, budget, text in SEGMENTS:
        dur = await synth(name, text, rate)
        total += dur
        budget_total += budget
        print(f"{name:<14}{dur:>7.1f}s{budget:>5}s{budget - dur:>7.1f}s")
    print(f"\n合计 {total:.1f}s（预算 {budget_total}s，目标 ≤175s）")
    if total > 178:
        print("提示：总时长超了，可用 `python gen_voiceover.py +8%` 提语速重生成")


if __name__ == "__main__":
    asyncio.run(main())
