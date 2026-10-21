"""服务端控制面包：把仓库根目录放进 sys.path，保证契约包可被 import。"""
import sys
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[2]
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))
