import copy from 'copy-to-clipboard'
import * as H from 'history'
import CommentCheckIcon from 'mdi-react/CommentCheckIcon'
import CommentRemoveIcon from 'mdi-react/CommentRemoveIcon'
import FlagVariantIcon from 'mdi-react/FlagVariantIcon'
import LinkIcon from 'mdi-react/LinkIcon'
import SecurityLockIcon from 'mdi-react/SecurityLockIcon'
import * as React from 'react'
import { Link } from 'react-router-dom'
import { Observable } from 'rxjs'
import * as GQL from '../../../shared/src/graphql/schema'
import { asError } from '../../../shared/src/util/errors'
import { Markdown } from '../components/Markdown'
import { Timestamp } from '../components/time/Timestamp'
import { eventLogger } from '../tracking/eventLogger'
import { UserAvatar } from '../user/UserAvatar'

interface Props {
    comment: GQL.IDiscussionComment
    threadID: GQL.ID
    location: H.Location

    /**
     * Whether or not the user is a site administrator.
     */
    isSiteAdmin: boolean

    /**
     * When specified, a report icon will be displayed inline and this function
     * will be called when a report has been submitted.
     */
    onReport?: (comment: GQL.IDiscussionComment, reason: string) => Observable<void>

    /**
     * When specified, this function is called to handle the
     * "Clear reports / mark as read" button clicks.
     */
    onClearReports?: (comment: GQL.IDiscussionComment) => Observable<void>

    /**
     * When specified, this function is called to handle the "delete comment"
     * button clicks.
     */
    onDelete?: (comment: GQL.IDiscussionComment) => Observable<void>
}

interface State {
    copiedLink: boolean
}

export class DiscussionsComment extends React.PureComponent<Props> {
    private scrollToElement: HTMLElement | null = null

    public state: State = {
        copiedLink: false,
    }

    public componentDidMount(): void {
        if (this.scrollToElement) {
            this.scrollToElement.scrollIntoView()
        }
    }

    public render(): JSX.Element | null {
        const { location, comment, isSiteAdmin, onReport, onClearReports, onDelete } = this.props
        const isTargeted = new URLSearchParams(location.hash).get('commentID') === comment.id

        // TODO(slimsag:discussions): ASAP: markdown links, headings, etc lead to #

        return (
            <div
                className={`discussions-comment${isTargeted ? ' discussions-comment--targeted' : ''}`}
                ref={isTargeted ? this.setScrollToElement : undefined}
            >
                <div className="discussions-comment__top-area">
                    <span className="discussions-comment__author">
                        <Link to={`/users/${comment.author.username}`} data-tooltip={comment.author.displayName}>
                            <UserAvatar user={comment.author} className="icon-inline icon-sm" />
                        </Link>
                        <Link
                            to={`/users/${comment.author.username}`}
                            data-tooltip={comment.author.displayName}
                            className="ml-1 mr-1"
                        >
                            {comment.author.username}
                        </Link>
                        <span className="mr-1">commented</span>
                        <Timestamp date={comment.createdAt} />
                    </span>
                    <span className="discussions-comment__spacer" />
                    <span className="discussions-comment__top-right-area">
                        {this.props.comment.inlineURL && (
                            <Link
                                className="btn btn-link btn-sm discussions-comment__share"
                                data-tooltip="Copy link to this comment"
                                to={this.props.comment.inlineURL}
                                onClick={this.onShareLinkClick}
                            >
                                {this.state.copiedLink ? 'Copied!' : <LinkIcon className="icon-inline" />}
                            </Link>
                        )}

                        {comment.canReport &&
                            onReport && (
                                <button
                                    className="btn btn-link btn-sm discussions-comment__report"
                                    data-tooltip="Report this comment"
                                    onClick={this.onReportClick}
                                >
                                    <FlagVariantIcon className="icon-inline" />
                                </button>
                            )}
                        {isSiteAdmin && (
                            <span className="discussions-comment__admin">
                                <SecurityLockIcon className="icon-inline icon-sm" data-tooltip="Admin area" />
                                {comment.reports.length > 0 && (
                                    <>
                                        <span
                                            className="ml-1 mr-1 discussions-comment__reports"
                                            data-tooltip={comment.reports.join('\n\n')}
                                        >
                                            {comment.reports.length} reports
                                        </span>
                                        {comment.canClearReports &&
                                            onClearReports && (
                                                <button
                                                    className="btn btn-link btn-sm discussions-comment__toolbar-btn"
                                                    data-tooltip="Clear reports / mark as good message"
                                                    onClick={this.onClearReportsClick}
                                                >
                                                    <CommentCheckIcon className="icon-inline" />
                                                </button>
                                            )}
                                    </>
                                )}
                                {comment.canDelete &&
                                    onDelete && (
                                        <button
                                            className="btn btn-link btn-sm discussions-comment__toolbar-btn"
                                            data-tooltip="Delete comment forever"
                                            onClick={this.onDeleteClick}
                                        >
                                            <CommentRemoveIcon className="icon-inline" />
                                        </button>
                                    )}
                            </span>
                        )}
                    </span>
                </div>
                <div className="discussions-comment__content">
                    <Markdown dangerousInnerHTML={comment.html} />
                </div>
            </div>
        )
    }

    private onShareLinkClick: React.MouseEventHandler<HTMLElement> = event => {
        if (event.metaKey || event.altKey || event.ctrlKey) {
            return
        }
        eventLogger.log('ShareCommentButtonClicked')
        event.preventDefault()
        copy(this.props.comment.inlineURL!) // ! because this method is only called when inlineURL exists
        this.setState({ copiedLink: true })
        setTimeout(() => {
            this.setState({ copiedLink: false })
        }, 1000)
    }

    private onReportClick: React.MouseEventHandler<HTMLElement> = event => {
        eventLogger.log('ReportCommentButtonClicked')
        const reason = prompt('Report reason:', 'spam, offensive material, etc')
        if (!reason) {
            return
        }
        this.props.onReport!(this.props.comment, reason).subscribe(
            undefined,
            error => (error ? alert('Error reporting comment: ' + asError(error).message) : undefined)
        )
    }

    private onClearReportsClick: React.MouseEventHandler<HTMLElement> = event => {
        this.props.onClearReports!(this.props.comment).subscribe(
            undefined,
            error => (error ? alert('Error clearing comment reports: ' + asError(error).message) : undefined)
        )
    }

    private onDeleteClick: React.MouseEventHandler<HTMLElement> = event => {
        this.props.onDelete!(this.props.comment).subscribe(
            undefined,
            error => (error ? alert('Error deleting comment: ' + asError(error).message) : undefined)
        )
    }

    private setScrollToElement = (ref: HTMLElement | null) => {
        this.scrollToElement = ref
    }
}
