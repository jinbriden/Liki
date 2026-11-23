import { App, Button, Empty, Input, Modal, Spin, Tag, Typography } from 'antd'
import { useEffect, useState } from 'react'
import type { AccountStatus } from '../api/auth'
import { errorMessage } from '../api/client'
import {
  createInvite,
  inviteLink,
  listInvites,
  MAX_INVITE_NOTE_LEN,
  revokeInvite,
  type Invite,
  type InviteState,
  type VerifyResult,
} from '../api/invite'
import { formatTime } from '../utils/time'

// InviteModal 生成邀请链接、查看邀请码的使用情况（FR-23）。只有 canInvite 的账号能打开（现阶段只有管理员）。
// 审核不在这里：注册后的账号仍要在服务器上用 im-admin 批准（FR-22）
export default function InviteModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    // destroyOnHidden：关闭后卸载内容，下次打开时重新拉取列表
    <Modal open={open} title="邀请注册" footer={null} onCancel={onClose} destroyOnHidden width={560}>
      <InviteContent />
    </Modal>
  )
}

const STATE_TAG: Record<InviteState, { text: string; color?: string }> = {
  active: { text: '可用', color: 'success' },
  reserved: { text: '冻结中', color: 'warning' }, // 有人验证通过，正在填写注册表单
  used: { text: '已使用' },
  expired: { text: '已过期' },
  revoked: { text: '已作废' },
}

const VERIFY_TEXT: Record<VerifyResult, string> = {
  reserved: '验证成功',
  invalid: '无效',
  in_use: '使用中被拒',
  limited: '太频繁被拒',
}

const ACCOUNT_TAG: Record<AccountStatus, { text: string; color?: string }> = {
  pending: { text: '待审核' },
  active: { text: '已通过', color: 'success' },
  rejected: { text: '已拒绝', color: 'error' },
}

function InviteContent() {
  const { message } = App.useApp()
  const [invites, setInvites] = useState<Invite[] | null>(null) // null 表示还在加载
  const [loadError, setLoadError] = useState<string | null>(null)
  const [note, setNote] = useState('')
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<Invite | null>(null) // 刚生成的那个，单独突出显示

  useEffect(() => {
    let cancelled = false
    listInvites()
      .then((items) => {
        if (!cancelled) setInvites(items)
      })
      .catch((err: unknown) => {
        if (!cancelled) setLoadError(errorMessage(err))
      })
    return () => {
      cancelled = true
    }
  }, [])

  async function onCreate() {
    setCreating(true)
    try {
      const inv = await createInvite(note.trim())
      setCreated(inv)
      setInvites((list) => [inv, ...(list ?? [])])
      setNote('')
    } catch (err) {
      message.error(errorMessage(err))
    } finally {
      setCreating(false)
    }
  }

  function replace(inv: Invite) {
    setInvites((list) => list?.map((x) => (x.id === inv.id ? inv : x)) ?? null)
    setCreated((c) => (c?.id === inv.id ? inv : c))
  }

  return (
    <>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 12 }}>
        每个邀请码 3 天内有效，只能提交一次注册申请。对方提交后，仍需你在服务器上审核通过才能登录。
      </Typography.Paragraph>
      <div style={{ display: 'flex', gap: 8 }}>
        <Input
          placeholder="备注（选填），如：给小王"
          maxLength={MAX_INVITE_NOTE_LEN}
          value={note}
          onChange={(e) => setNote(e.target.value)}
          onPressEnter={() => void onCreate()}
        />
        <Button type="primary" loading={creating} onClick={() => void onCreate()}>
          生成邀请链接
        </Button>
      </div>

      {created && created.state === 'active' && (
        <div className="invite-created">
          <div className="invite-created-code">{created.code}</div>
          <Typography.Text copyable={{ text: inviteLink(created.code), tooltips: ['复制邀请链接', '已复制'] }} ellipsis>
            {inviteLink(created.code)}
          </Typography.Text>
        </div>
      )}

      <div className="invite-list-title">已生成的邀请码</div>
      <div className="invite-list">
        {loadError && <Typography.Text type="danger">{loadError}</Typography.Text>}
        {!loadError && invites === null && (
          <div style={{ textAlign: 'center', padding: 24 }}>
            <Spin />
          </div>
        )}
        {invites?.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有生成过邀请码" />}
        {invites?.map((inv) => <InviteItem key={inv.id} invite={inv} onChange={replace} />)}
      </div>
    </>
  )
}

function InviteItem({ invite, onChange }: { invite: Invite; onChange: (inv: Invite) => void }) {
  const { message, modal } = App.useApp()
  const [showVerifications, setShowVerifications] = useState(false)
  const tag = STATE_TAG[invite.state]
  const usable = invite.state === 'active' || invite.state === 'reserved'

  async function copyLink() {
    try {
      await navigator.clipboard.writeText(inviteLink(invite.code))
      message.success('已复制邀请链接')
    } catch {
      message.error('复制失败，请手动复制邀请码')
    }
  }

  function confirmRevoke() {
    modal.confirm({
      title: `作废邀请码 ${invite.code}？`,
      content:
        invite.state === 'reserved'
          ? '有人正在用这个邀请码填写注册表单，作废后对方将无法提交。'
          : '作废后这个邀请码不能再用于注册。',
      okText: '作废',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          onChange(await revokeInvite(invite.id))
        } catch (err) {
          message.error(errorMessage(err))
        }
      },
    })
  }

  return (
    <div className="card-item">
      <div className="card-item-main">
        <div className="card-item-name">
          <span className="invite-code">{invite.code}</span>
          <Tag color={tag.color} variant="filled" style={{ marginInlineEnd: 0 }}>
            {tag.text}
          </Tag>
          {invite.note && <span className="card-item-id">{invite.note}</span>}
        </div>
        <div className="card-item-time">
          生成于 {formatTime(invite.createdAt)}
          {usable && ` · ${formatTime(invite.expiresAt)} 过期`}
          {invite.state === 'reserved' && invite.reservedUntil && ` · 冻结到 ${formatTime(invite.reservedUntil)}`}
          {invite.verifications.length > 0 && (
            <>
              {' · '}
              <Typography.Link style={{ fontSize: 12 }} onClick={() => setShowVerifications((v) => !v)}>
                验证记录 {invite.verifications.length}
              </Typography.Link>
            </>
          )}
        </div>
        {/* 溯源：用这个码提交注册申请的账号（一个码只能提交一次），以及它的审核状态 */}
        {invite.usedBy && (
          <div className="invite-invitee">
            <span>{invite.usedBy.username}</span>
            <span className="card-item-id">ID {invite.usedBy.id}</span>
            <Tag color={ACCOUNT_TAG[invite.usedBy.status].color} variant="filled" style={{ marginInlineEnd: 0 }}>
              {ACCOUNT_TAG[invite.usedBy.status].text}
            </Tag>
          </div>
        )}
        {showVerifications && (
          <div className="invite-verifications">
            {invite.verifications.map((v, i) => (
              <div key={i}>
                {formatTime(v.createdAt)} · {v.ip} · {VERIFY_TEXT[v.result]}
              </div>
            ))}
          </div>
        )}
      </div>
      {usable && (
        <div className="card-item-actions">
          {invite.state === 'active' && (
            <Button size="small" onClick={() => void copyLink()}>
              复制链接
            </Button>
          )}
          <Button size="small" type="text" danger onClick={confirmRevoke}>
            作废
          </Button>
        </div>
      )}
    </div>
  )
}
