"""Native canary subprocess: actual knowledge Brain/Journal, fake Feishu only.

The caller freezes the third repository, supplies a new host-only state folder
and exposes this command only through the real gateway adapter. No production
configuration, credentials or services are loaded.
"""
import argparse
import json
from pathlib import Path
import sys


REVIEW_NODE = "syntheticreview"
REVIEW_TITLE = "虚构会后复盘模板"
REVIEW_GUIDANCE = "记录具体事件、原句和出处；未知事项保持待确认。"
REVIEW_CASE = "客户标识：待填写；沟通时间：待填写；负责人：待填写。"
REVIEW_ACTION = "会后选一个下次尝试的改进动作，并记录观察结果。"
REVIEW_COLLECTED = "团队假设（待验证）：" + REVIEW_ACTION


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True)
    parser.add_argument("--state", required=True)
    args = parser.parse_args()
    root, state = Path(args.root), Path(args.state)
    if not root.is_absolute() or not state.is_absolute() or not state.is_dir():
        raise RuntimeError("isolated source and state must already exist")
    sys.path.insert(0, str(root))
    from team_brain.contracts import BrainError, Principal, paragraph, strict_json
    from team_brain.host import dispatch
    from team_brain.journal import Journal
    from team_brain.service import Brain, bodyprint
    from tests.fakes import FakeFeishu

    control_path = state / "knowledge-template-evidence.json"
    control = None
    if control_path.exists():
        control = strict_json(control_path.read_text(encoding="utf-8"))
        if (not isinstance(control, dict) or
                not (control == {"phase": "template"} or
                     (set(control) == {"phase", "approval_id"} and control["phase"] == "collected" and
                      isinstance(control["approval_id"], str) and control["approval_id"]))):
            raise RuntimeError("invalid host template evidence phase")

    class QueryFeishu(FakeFeishu):
        def search(self, query, page_token=""):
            if control is not None and "复盘" in query:
                return {"nodes": [REVIEW_NODE], "next_page_token": ""}
            if "青松" not in query:
                return {"nodes": [], "next_page_token": ""}
            # A filtered-out first native page must not be mistaken for no data.
            return {"nodes": ["syntheticqingsong"] if page_token else ["unpublished"],
                    "next_page_token": "" if page_token else "synthetic-next"}

    api = QueryFeishu()
    node = api.create_node("虚构系统访谈")["node_token"]
    api.append(node, [paragraph("伙伴转述：系统账号约 20 个。目前只能导出 Excel，API 能力尚未确认。")], "fixture-seed")
    page = api.pages.pop(node)
    page.update(node_token="syntheticqingsong", url=api.url("syntheticqingsong"),
                unsupported_blocks=[{"block_type": 27, "reason": "table_not_parsed"}])
    api.pages["syntheticqingsong"] = page
    node = api.create_node("虚构初访方法")["node_token"]
    api.append(node, [paragraph("先询问最近一笔业务从开始到结束的过程，再确认返工或等待发生的位置；最后问什么结果能证明改善有效。")], "fixture-method")
    method = api.pages.pop(node)
    method.update(node_token="syntheticmethod", url=api.url("syntheticmethod"))
    api.pages["syntheticmethod"] = method
    principals = [Principal("feishu", "sender-1", "group-1", "feishu:group-1:sender-1", "test")]
    principals.append(Principal("feishu", "sender-1", "private-0", "private-session-0", "kb-private-0"))
    principals += [Principal("feishu", "fixture-" + str(i), kind + "-" + str(i),
                             kind + "-session-" + str(i), "kb-" + kind + "-" + str(i))
                   for i in (1, 2) for kind in ("private", "group")]
    journal = Journal(state / "knowledge.sqlite3")
    try:
        envelope = strict_json(sys.stdin.read(65537))
        brain = Brain(journal, api, principals)

        def validate_collected_proposal():
            principal = Principal.parse(envelope["principal"])
            if envelope["operation"] != "tool" or principal.key != principals[0].key:
                raise BrainError("invalid_template_evidence_precondition")
            proposal = journal.get(control["approval_id"])
            if not (proposal["id"] == control["approval_id"] and
                    proposal["actor"] == principal.key and
                    proposal["owner"] == brain.owner(principal, envelope["session_token"]) and
                    proposal["target"] == REVIEW_NODE and proposal["subject"] == "team" and
                    proposal["state"] == "pending" and
                    isinstance(proposal["card"], str) and proposal["card"] and
                    proposal["receipt"] is None and proposal["step"] == "" and
                    not proposal["block_id"] and REVIEW_ACTION in proposal["content"]):
                raise BrainError("invalid_template_evidence_precondition")

        if control is not None:
            if control["phase"] == "collected":
                validate_collected_proposal()
            # The host rebuilds only fake page data; the real proposal remains pending.
            node = api.create_node(REVIEW_TITLE)["node_token"]
            api.append(node, [paragraph(REVIEW_GUIDANCE), paragraph(REVIEW_CASE)], "fixture-review-template")
            if control["phase"] == "collected":
                api.append(node, [paragraph(REVIEW_COLLECTED)], "fixture-review-collected")
            review = api.pages.pop(node)
            review.update(node_token=REVIEW_NODE, url=api.url(REVIEW_NODE),
                          unsupported_blocks=[{"block_type": 27, "reason": "table_not_parsed"}])
            api.pages[REVIEW_NODE] = review
        with journal.transaction():
            journal.register("syntheticqingsong", "演示·青松B")
            journal.baseline("syntheticqingsong", bodyprint(api.snapshot("syntheticqingsong")))
            journal.register("syntheticmethod", "team")
            journal.baseline("syntheticmethod", bodyprint(api.snapshot("syntheticmethod")))
            if control is not None:
                journal.register(REVIEW_NODE, "team")
                journal.baseline(REVIEW_NODE, bodyprint(api.snapshot(REVIEW_NODE)))
        writes = api.writes
        result = dispatch(brain, envelope)
        if api.writes != writes:
            raise RuntimeError("unexpected external write")
        if control is not None and control["phase"] == "collected":
            validate_collected_proposal()
        print(json.dumps(result, ensure_ascii=False))
    except BrainError as exc:
        print(json.dumps({"status": "blocked", "code": exc.code}))
    finally:
        journal.close()


if __name__ == "__main__":
    main()
