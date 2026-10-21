"""隐私分档策略：三档规则只写一处，编排与网关都来问这里，避免两边各解释一遍。"""
from __future__ import annotations

from typing import Final

LOCAL: Final[str] = "local"            # L1 纯本地工具，文件内容不出本机
STANDARD: Final[str] = "standard"      # L2 服务端编排 + 模型，只落摘要
SENSITIVE: Final[str] = "sensitive"    # L3 执行器直连模型，内容不过公司服务器

MODEL_TIERS: Final[frozenset] = frozenset({STANDARD})


class PolicyViolation(RuntimeError):
    """策略违规单列一类异常：调用方捕获它即可，不必认识各档细节。"""


class PrivacyPolicy:
    """task_type → 隐私档，并守住"哪一档允许服务端模型调用"这条红线。"""

    def classify(self, task_type: str) -> str:
        """规则少到可以一眼审完：local/sensitive 显式命中，其余落 standard。"""
        key = (task_type or "").strip().lower()
        if key == LOCAL:
            return LOCAL
        if key == SENSITIVE:
            return SENSITIVE
        return STANDARD

    def allows_model_call(self, task_type: str) -> bool:
        """只有 standard 档能进服务端模型：L1 不过大脑，L3 由执行器直连。"""
        return self.classify(task_type) in MODEL_TIERS

    def validate(self, model_call_allowed: bool, task_type: str) -> None:
        """违规立刻抛，不写"记个日志继续跑"的降级路径——隐私红线没有降级。"""
        if not model_call_allowed:
            return
        tier = self.classify(task_type)
        if tier == LOCAL:
            raise PolicyViolation(f"L1(local) 档禁止模型调用，task_type={task_type}")
        if tier == SENSITIVE:
            raise PolicyViolation(
                f"L3(sensitive) 档禁止经服务端模型调用，应走执行器直连，task_type={task_type}"
            )
