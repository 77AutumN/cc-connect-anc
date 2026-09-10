"""Native canary subprocess: actual knowledge Brain/Journal, fake Feishu only.

The caller freezes the third repository, supplies a new host-only state folder
and exposes this command only through the real gateway adapter. No production
configuration, credentials or services are loaded.
"""
import argparse
import json
from pathlib import Path
import sys


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

    class QueryFeishu(FakeFeishu):
        def search(self, query, page_token=""):
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
    principals = [Principal("feishu", "sender-1", "group-1", "feishu:group-1:sender-1", "test")]
    principals.append(Principal("feishu", "sender-1", "private-0", "private-session-0", "kb-private-0"))
    principals += [Principal("feishu", "fixture-" + str(i), kind + "-" + str(i),
                             kind + "-session-" + str(i), "kb-" + kind + "-" + str(i))
                   for i in (1, 2) for kind in ("private", "group")]
    journal = Journal(state / "knowledge.sqlite3")
    try:
        with journal.transaction():
            journal.register("syntheticqingsong", "演示·青松B")
            journal.baseline("syntheticqingsong", bodyprint(api.snapshot("syntheticqingsong")))
        writes = api.writes
        result = dispatch(Brain(journal, api, principals), strict_json(sys.stdin.read(65537)))
        if api.writes != writes:
            raise RuntimeError("unexpected external write")
        print(json.dumps(result, ensure_ascii=False))
    except BrainError as exc:
        print(json.dumps({"status": "blocked", "code": exc.code}))
    finally:
        journal.close()


if __name__ == "__main__":
    main()
